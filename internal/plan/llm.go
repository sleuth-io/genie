// Package plan generates monty scripts on demand for nodes whose shape isn't
// crystallised yet. The single entry point is Generator.Generate, which:
//
//  1. Builds a prompt from (node, parent context, MCP tool catalog)
//  2. Calls Claude (Opus 4.7, adaptive thinking, prompt-cached tool catalog)
//  3. Parses the JSON response
//  4. Persists the result into the crystallize Store
//  5. Returns the script
//
// The MCP tool catalog goes in the system prompt with a cache breakpoint, so
// repeated generations within a single CLI invocation pay full input cost
// once and then ~0.1× thereafter.
package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sleuth-io/genie/internal/crystallize"
	"github.com/sleuth-io/genie/internal/engine"
	"github.com/sleuth-io/genie/internal/fixtures"
	"github.com/sleuth-io/genie/internal/llm"
	"github.com/sleuth-io/genie/internal/mcpclient"
	"github.com/sleuth-io/genie/internal/progress"
	"github.com/sleuth-io/genie/internal/runtime"
	"github.com/sleuth-io/genie/internal/session"
)

// Generator wires an Anthropic client + tool catalog + crystallize store
// into the engine.ScriptGenerator interface. Construct one per CLI run; the
// rendered tool catalog is built once at construction time and reused
// across every Generate call (which is exactly what makes the prompt cache
// useful).
//
// Generate orchestrates two distinct LLM calls on a cache miss:
//
//  1. NORMALIZE — small call, produces a canonical schema JSON.
//     Hash it; look up the L2 entry. If present, done — saves the
//     generate call entirely (the load-bearing hypothesis-2 win).
//  2. GENERATE — big call, produces the monty script + io_schema.
//     Only fires if L2 is also a miss.
type Generator struct {
	client                llm.Client
	mcp                   *mcpclient.Client // upstream caller, used by the tool-use loop
	monty                 *runtime.MontyEngine
	caps                  *runtime.Capabilities
	store                 *crystallize.Store
	generateSystemToolUse []llm.SystemBlock
	normalizeSystem       []llm.SystemBlock
	toolNames             []string // monty-side names like github_list_pull_requests
	tools                 []mcp.Tool
	provider              string // routing key — recorded in session entries
	session               *session.Session
	normalizeModel        string // GENIE_NORMALIZE_MODEL — empty ⇒ backend default
	generateModel         string // GENIE_GENERATE_MODEL — empty ⇒ backend default

	// metrics for hypothesis-2 measurement
	metrics Metrics
}

// Metrics is the running counter the eval harness reads after each query.
// Cumulative across the lifetime of the Generator — eval harness snapshots
// before/after each case to derive per-case deltas.
type Metrics struct {
	NormalizeCalls           int
	GenerateCalls            int
	NormalizeInputTokens     int64
	NormalizeOutputTokens    int64
	GenerateInputTokens      int64
	GenerateOutputTokens     int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// Snapshot returns a copy of the current Metrics. Safe to use as a
// before/after diff.
func (g *Generator) Snapshot() Metrics {
	return g.metrics
}

// TotalLLMTokens is input + output across both call types.
func (m Metrics) TotalLLMTokens() int64 {
	return m.NormalizeInputTokens + m.NormalizeOutputTokens +
		m.GenerateInputTokens + m.GenerateOutputTokens
}

// NormalizeOnly runs just the NORMALIZE LLM call and returns the canonical
// hash. Used by the adversarial-fingerprint eval (Day 7) to compare cache
// keys across query pairs without spending tokens on full generation.
func (g *Generator) NormalizeOnly(ctx context.Context, n *engine.Node) (string, error) {
	_, hash, _, _, err := g.normalizeNode(ctx, n)
	return hash, err
}

// NewGenerator initialises the generator. Call once per (provider, store)
// scope. The LLM client is injected so callers can pick a backend
// (Anthropic SDK, Claude Code CLI, …) per the rules in package llm.
// sess is the JSONL session log; pass nil to skip recording.
//
// Per-call-type model overrides are read from env at construction:
// GENIE_NORMALIZE_MODEL and GENIE_GENERATE_MODEL. NORMALIZE produces
// small structured output (canonical schema + rename maps) and runs
// fine on Sonnet/Haiku; GENERATE writes Python and may benefit from
// Opus. Empty values keep the backend's built-in default (Opus 4.7
// on the Anthropic SDK; Claude Code's session default for the CLI).
func NewGenerator(c *mcpclient.Client, store *crystallize.Store, llmClient llm.Client, provider string, sess *session.Session) *Generator {
	tools := c.Tools()
	catalog := renderToolCatalog(tools)

	return &Generator{
		client:                llmClient,
		mcp:                   c,
		store:                 store,
		generateSystemToolUse: buildGenerateSystemToolUse(catalog),
		normalizeSystem:       buildNormalizeSystem(catalog),
		toolNames:             c.MontyToolNames(),
		tools:                 tools,
		provider:              provider,
		session:               sess,
		normalizeModel:        os.Getenv("GENIE_NORMALIZE_MODEL"),
		generateModel:         os.Getenv("GENIE_GENERATE_MODEL"),
	}
}

// WithRunner attaches the monty engine and base capabilities used by
// the tool-use GENERATE flow's verification step. Required —
// Generate errors out if no runner is wired.
//
// Returns the receiver so the constructor can be chained.
func (g *Generator) WithRunner(monty *runtime.MontyEngine, caps *runtime.Capabilities) *Generator {
	g.monty = monty
	g.caps = caps
	return g
}

// callLLM is a thin wrapper around g.client.Generate that records
// the request/response pair to the JSONL session log. callType is
// "normalize" or "generate"; field is the GraphQL field name being
// resolved (for context when reading the log).
func (g *Generator) callLLM(ctx context.Context, callType, field string, system []llm.SystemBlock, userText string) (llm.Response, error) {
	switch callType {
	case "normalize":
		progress.Report(ctx, "Normalizing %q…", field)
		ctx = llm.WithModel(ctx, g.normalizeModel)
		// NORMALIZE produces a small, structured-shape JSON object
		// (canonical schema + rename maps). It's a constraint-
		// satisfying transformation, not a reasoning task — adaptive
		// thinking adds latency without measurable accuracy gain.
		// Hyp-3 (paraphrase fingerprinting) covers this empirically.
		ctx = llm.WithEffort(ctx, llm.EffortDisabled)
	case "generate":
		progress.Report(ctx, "Generating script for %q…", field)
		ctx = llm.WithModel(ctx, g.generateModel)
	default:
		progress.Report(ctx, "%s %q…", callType, field)
	}
	start := time.Now()
	resp, err := g.client.Generate(ctx, system, userText)
	rec := session.Record{
		Call:       callType,
		Provider:   g.provider,
		Field:      field,
		SystemText: joinBlocks(system),
		UserText:   userText,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		rec.Err = err.Error()
	} else {
		rec.Response = resp.Text
		rec.Usage = &session.Usage{
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
		}
	}
	g.session.AppendCtx(ctx, rec)
	return resp, err
}

// synthesizeProjection returns a deterministic monty script when n is
// safe to skip GENERATE for: a non-top-level object selection whose
// children are all scalar leaves over a parent in hand. The script
// projects the requested canonical keys out of the parent dict (or
// list of dicts) the parent script returned. Returns ok=false if any
// check fails — caller falls through to the LLM-driven GENERATE path.
//
// Why this is safe regardless of literal-vs-canonical names:
//
//   - The parent script is canonical-keyed (the GENERATE call that
//     produced it was instructed to use canonical names per the
//     normalize step's canonical_schema). So the parent dict's keys
//     ARE the canonical names of its children.
//   - This synthesized script also reads canonical names. The
//     literal-to-canonical translation happens in the engine's
//     rename projection AROUND this script, not inside it.
//   - The synthesized output IS what GENERATE would have written:
//     `parent.get(canonical_key)` per scalar leaf. No reasoning
//     involved on either path.
//
// What we deliberately don't synthesize:
//
//   - Top-level nodes (parent == nil) — they need to invoke MCP
//     tools; only the LLM knows which.
//   - Nodes with args — the args might select a different upstream
//     code path; can't predict from name alone.
//   - Children with their own selection — recurse normally; the
//     child's own Generate call will decide whether IT is trivial.
//   - Children with their own args — same reason as the parent
//     having args: arg values may steer a different shape.
//
// Earlier this function also bailed when any child rename was
// non-identity. That check is dropped — but we DO have to handle
// vocabulary divergence between the parent's GENERATE and the
// child's NORMALIZE, which can pick different canonical names for
// the same logical field.
//
// Concrete divergence we hit in production:
//   - Parent script (GENERATE) projects `{"updater": {"displayName": ...}}`
//     using the user's literal names, since the LLM at GENERATE
//     time doesn't run NORMALIZE on grandchildren.
//   - Child NORMALIZE for `last_updater` independently picks
//     canonical name `display_name`.
//   - Synthesize with only one key would read `parent["display_name"]`,
//     find nothing, return null silently.
//
// Fix: emit a multi-key fallback in the synthesized script. Try
// both the user's literal name (what the parent script likely
// projected) and the child's own canonical (the rename map's
// target). They're often the same; when they differ, the fallback
// catches whichever the parent actually wrote.
//
// The OUTPUT key is canonical so the engine's rename projection
// (which translates canonical→literal at compose time) still
// produces the user-facing literal name.
type synthKey struct {
	out     string   // canonical name the script writes (engine renames this back to literal)
	tryRead []string // ordered list of keys to try reading from parent
}

func (g *Generator) synthesizeProjection(n *engine.Node, parent any, rename *engine.NodeRename) (string, any, bool) {
	if parent == nil {
		return "", nil, false
	}
	if len(n.Args) > 0 {
		return "", nil, false
	}
	if len(n.Selection) == 0 {
		return "", nil, false
	}
	// If the parent's value at this node is a scalar but the user
	// requested an object selection (e.g. `author { displayName }`
	// where the parent script projected `author: "Dylan Etkin"`),
	// the synthesized projection would emit `parent.get("displayName")`
	// against a string and crash. Bail out and let the tool-use
	// generate path write a script that handles the wrap (or
	// refuses) — that path can return `{requested_field: parent}`
	// when the parent IS the human-readable value the user asked
	// for.
	switch parent.(type) {
	case map[string]any, []any:
		// fine — these are the shapes the synthesised projection
		// already handles.
	default:
		return "", nil, false
	}
	keys := make([]synthKey, 0, len(n.Selection))
	for _, child := range n.Selection {
		if len(child.Selection) > 0 || len(child.Args) > 0 {
			return "", nil, false
		}
		canon := child.Name
		if rename != nil {
			if c, ok := rename.ChildrenLiteralToCanonical[child.Name]; ok {
				canon = c
			}
		}
		// Build read-attempt list: literal first (most common —
		// LLM-generated parent scripts use literals), canonical
		// second, deduped.
		reads := []string{child.Name}
		if canon != child.Name {
			reads = append(reads, canon)
		}
		keys = append(keys, synthKey{out: canon, tryRead: reads})
	}

	// Build the projection literal. Each entry is
	//     "<canonical>": _d.get("<literal>") or _d.get("<canonical>")
	// where _d is whichever dict variable is in scope. Monty's
	// sandboxed Python doesn't expose module-level helpers to
	// execute()'s scope, so we inline rather than emitting a
	// _pick helper.
	pick := func(varName string) string {
		var b strings.Builder
		b.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q: ", k.out)
			for j, r := range k.tryRead {
				if j > 0 {
					b.WriteString(" or ")
				}
				fmt.Fprintf(&b, "%s.get(%q)", varName, r)
			}
		}
		b.WriteString("}")
		return b.String()
	}
	itemPick := pick("item")
	parentPick := pick("parent")

	var script strings.Builder
	script.WriteString("def execute(args, parent):\n")
	script.WriteString("    if isinstance(parent, list):\n")
	fmt.Fprintf(&script, "        return [%s for item in parent if isinstance(item, dict)]\n", itemPick)
	script.WriteString("    if isinstance(parent, dict):\n")
	fmt.Fprintf(&script, "        return %s\n", parentPick)
	script.WriteString("    return None\n")

	properties := map[string]any{}
	for _, k := range keys {
		properties[k.out] = map[string]any{}
	}
	ioSchema := map[string]any{
		"type":        "object",
		"properties":  properties,
		"synthesized": true,
	}
	return script.String(), ioSchema, true
}

func joinBlocks(blocks []llm.SystemBlock) string {
	var b strings.Builder
	for i, blk := range blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// Generate is engine.ScriptGenerator. On each call:
//
//  1. Re-check L1 (the executor already checked but a sibling node may
//     have written through during this query).
//  2. NORMALIZE: small LLM call produces canonical_schema + per-call rename.
//  3. Look up L2 by canonical_hash. Hit → write L1 alias, return script + rename.
//  4. GENERATE: big LLM call produces monty_script + io_schema.
//  5. Write L2 entry + L1 alias. Return script + rename.
//
// The returned NodeRename is non-nil for L2 hits and full GEN. The Day-6
// engine applies it around the script invocation; without it, paraphrases
// that rename fields/args produce null-valued composition.
func (g *Generator) Generate(ctx context.Context, n *engine.Node, parent any) (string, *engine.NodeRename, error) {
	shape := n.Shape()
	literalHash := shape.L1Hash()

	// Step 1: L1 hit — skip normalization entirely. The cached script is
	// canonical-keyed; the rename stored on the alias tells the engine how
	// to translate this literal's args/fields around it.
	if alias, ok, _ := g.store.GetAlias(literalHash); ok {
		if entry, ok, _ := g.store.GetEntry(alias.CanonicalHash); ok {
			g.session.AppendCtx(ctx, session.Record{
				Call: "cache_l1", Provider: g.provider, Field: n.Name,
				Hit: true, Hash: literalHash[:12],
			})
			return entry.MontyScript, alias.Rename, nil
		}
	}
	g.session.AppendCtx(ctx, session.Record{
		Call: "cache_l1", Provider: g.provider, Field: n.Name,
		Hit: false, Hash: literalHash[:12],
	})

	// Step 2: normalize.
	canonical, canonicalHash, rename, canonSchema, err := g.normalizeNode(ctx, n)
	if err != nil {
		return "", nil, fmt.Errorf("normalize: %w", err)
	}

	// Step 3: L2 lookup.
	if script, _, ok := g.store.ResolveCanonical(canonicalHash); ok {
		slog.Info("plan: L2 hit (paraphrase)",
			"field", n.Name,
			"literal_hash", literalHash[:12],
			"canonical_hash", canonicalHash[:12],
		)
		g.session.AppendCtx(ctx, session.Record{
			Call: "cache_l2", Provider: g.provider, Field: n.Name,
			Hit: true, Hash: canonicalHash[:12],
		})
		if err := g.store.PutAlias(literalHash, canonicalHash, rename); err != nil {
			slog.Warn("plan: L1 alias write failed", "err", err)
		}
		return script, rename, nil
	}
	g.session.AppendCtx(ctx, session.Record{
		Call: "cache_l2", Provider: g.provider, Field: n.Name,
		Hit: false, Hash: canonicalHash[:12],
	})

	// Step 4a: short-circuit GENERATE for trivial-extraction nodes
	// — those whose children are scalar leaves with identity renames
	// over a known parent. The script we'd otherwise pay the LLM to
	// write is a deterministic projection; synthesize it directly,
	// persist as an L2 entry, return.
	if synth, ioSchema, ok := g.synthesizeProjection(n, parent, rename); ok {
		slog.Info("plan: synthesized trivial projection",
			"field", n.Name,
			"canonical_hash", canonicalHash[:12])
		g.session.AppendCtx(ctx, session.Record{
			Call: "synthesize", Provider: g.provider, Field: n.Name,
			Hash: canonicalHash[:12],
		})
		entry := crystallize.Entry{
			Shape:           shape,
			Field:           n.Name,
			CanonicalSchema: canonical,
			CanonicalHash:   canonicalHash,
			MontyScript:     synth,
			IOSchema:        ioSchema,
		}
		if err := g.store.PutEntry(entry); err != nil {
			slog.Warn("plan: L2 entry write failed; replay will regenerate",
				"field", n.Name, "canonical_hash", canonicalHash[:12], "err", err)
		}
		if err := g.store.PutAlias(literalHash, canonicalHash, rename); err != nil {
			slog.Warn("plan: L1 alias write failed", "err", err)
		}
		return synth, rename, nil
	}

	// Step 4: full generate. Pass the canonical schema so the LLM emits a
	// canonical-keyed script (other paraphrases reusing this script via L2
	// will then see consistent canonical output).
	//
	// Cold-cache GENERATE: explore-then-submit tool-use loop with
	// fixture-replay verification. Both LLM backends (SDK + claude
	// CLI) implement ToolUseDriver, and the runner is wired by
	// pkg/genie at construction time.
	driver, ok := g.client.(llm.ToolUseDriver)
	if !ok {
		return "", nil, fmt.Errorf("llm client does not implement ToolUseDriver")
	}
	if g.monty == nil || g.caps == nil {
		return "", nil, fmt.Errorf("generator has no runner wired (call WithRunner)")
	}
	_ = driver // type-asserted above; fullGenerateToolUse re-asserts
	out, fixtureSet, expectedOutput, err := g.fullGenerateToolUse(ctx, n, parent, canonSchema, rename)
	if err != nil {
		return "", nil, err
	}

	// Step 5: persist.
	entry := crystallize.Entry{
		Shape:           shape,
		Field:           n.Name,
		CanonicalSchema: canonical,
		CanonicalHash:   canonicalHash,
		MontyScript:     out.MontyScript,
		IOSchema:        out.IOSchema,
		ExpectedOutput:  expectedOutput,
	}
	if len(fixtureSet) > 0 {
		fixtureBytes, ferr := json.Marshal(fixtureSet)
		if ferr == nil {
			entry.Fixtures = fixtureBytes
		}
	}
	if err := g.store.PutEntry(entry); err != nil {
		slog.Warn("plan: L2 entry write failed; replay will regenerate",
			"field", n.Name, "canonical_hash", canonicalHash[:12], "err", err)
	}
	if err := g.store.PutAlias(literalHash, canonicalHash, rename); err != nil {
		slog.Warn("plan: L1 alias write failed", "err", err)
	}
	return out.MontyScript, rename, nil
}

// submitScriptToolName is the synthetic Anthropic tool the model
// must call to deliver its final monty_script. Distinct from any
// upstream MCP tool name so collisions are impossible.
const submitScriptToolName = "submit_script"

// fullGenerateToolUse drives the explore-then-submit GENERATE loop
// with in-loop verification. The model is given the upstream tool
// catalog as REAL Anthropic tools, calls them to learn response
// shapes, and finishes by calling submit_script with {script,
// expected_output, io_schema}. The engine then runs the submitted
// script against the captured fixtures, deep-diffs actual output vs
// expected_output, and either accepts (match) or feeds the diff back
// for one revision turn (mismatch). Verification is skipped when no
// runner is wired (g.monty/g.caps unset) — the LLM is trusted in
// that case.
//
// Returns:
//   - the parsed llmOutput (script + io_schema) the model submitted
//   - the fixture set captured during exploration (for L2 persist)
//   - the LLM-stated expected_output (for L2 persist)
func (g *Generator) fullGenerateToolUse(ctx context.Context, n *engine.Node, parent any, canon *canonicalSchema, rename *engine.NodeRename) (*llmOutput, fixtures.Set, any, error) {
	driver, ok := g.client.(llm.ToolUseDriver)
	if !ok {
		return nil, nil, nil, errors.New("llm client does not support tool-use")
	}
	if g.mcp == nil {
		return nil, nil, nil, errors.New("generator has no upstream mcp client wired")
	}

	userText, err := buildUserPrompt(n, parent, canon)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build user prompt: %w", err)
	}

	tools := g.buildToolDefs()
	messages := []llm.Message{{Role: "user", Text: userText}}
	// Revision policy: zero. The CLI driver's "revision" path
	// re-spawns claude with the original conversation + a "you got
	// it wrong" note appended, which forces the model to re-run the
	// entire exploration from scratch (claude --print is stateless
	// across invocations). The redo costs ~70-150s for a tiny
	// chance of fixing the script. With the prompt's nested-
	// selection guidance and synthesizeProjection's scalar bail,
	// the first submit is usually correct OR clearly wrong; in
	// either case we'd rather fail fast and let the caller retry
	// (which gets a fresh L1/L2 lookup) than burn another minute.
	const maxRevisions = 0

	verifyArgs := map[string]any{}
	for _, a := range n.Args {
		key := a.Name
		if rename != nil {
			if c, ok := rename.ArgsLiteralToCanonical[a.Name]; ok && c != "" {
				key = c
			}
		}
		verifyArgs[key] = a.Value
	}

	progress.Report(ctx, "Generating script for %q (tool-use)…", n.Name)
	ctx = llm.WithModel(ctx, g.generateModel)

	executor := &mcpToolExecutor{client: g.mcp, prefix: mcpclient.HostNamePrefix, providerPrefix: "mcp__" + g.provider + "__"}

	var fixtureSet fixtures.Set

	for revision := 0; revision <= maxRevisions; revision++ {
		start := time.Now()
		result, err := driver.Drive(ctx, llm.DriveRequest{
			System:         g.generateSystemToolUse,
			Messages:       messages,
			Tools:          tools,
			Executor:       executor,
			Provider:       g.provider,
			SubmitToolName: submitScriptToolName,
		})
		duration := time.Since(start).Milliseconds()

		// One session record summarising this Drive call. Per-tool
		// detail is captured separately via the tool_call records
		// emitted below (one per Observation).
		rec := session.Record{
			Call:       "generate",
			Provider:   g.provider,
			Field:      n.Name,
			DurationMS: duration,
		}
		if revision == 0 {
			rec.SystemText = joinBlocks(g.generateSystemToolUse)
			rec.UserText = userText
		}
		rec.Usage = &session.Usage{
			InputTokens:         result.Usage.InputTokens,
			OutputTokens:        result.Usage.OutputTokens,
			CacheCreationTokens: result.Usage.CacheCreationTokens,
			CacheReadTokens:     result.Usage.CacheReadTokens,
		}
		if result.Submit != nil {
			payload, _ := json.Marshal(result.Submit)
			rec.Response = "[submit] " + string(payload)
		} else if result.FinalText != "" {
			rec.Response = result.FinalText
		}
		if err != nil {
			rec.Err = err.Error()
		}
		g.session.AppendCtx(ctx, rec)

		logUsage("generate-toolloop", result.Usage)
		g.metrics.GenerateCalls++
		g.metrics.GenerateInputTokens += result.Usage.InputTokens
		g.metrics.GenerateOutputTokens += result.Usage.OutputTokens
		g.metrics.CacheReadInputTokens += result.Usage.CacheReadTokens
		g.metrics.CacheCreationInputTokens += result.Usage.CacheCreationTokens

		// Capture observations as fixtures (translate name to
		// script-side canonical: github_X) AND record one session
		// tool_call entry per observation for trace fidelity.
		//
		// Filter out claude-code built-ins (Bash, ToolSearch, Agent,
		// Read, …) — --allowedTools is a positive allowlist that
		// doesn't actually exclude built-ins, so they leak into the
		// stream. We drop them at capture time so they don't pollute
		// L2 fixtures or get attempted during verification replay.
		// The script-side allowlist is g.toolNames (host names with
		// the github_ prefix); anything outside it is noise.
		upstreamSet := make(map[string]struct{}, len(g.toolNames))
		for _, n := range g.toolNames {
			upstreamSet[n] = struct{}{}
		}
		fixtureSet = fixtureSet[:0]
		for _, obs := range result.Observations {
			scriptName := translateObservationName(obs.ToolName, g.provider)
			if _, ok := upstreamSet[scriptName]; !ok {
				continue
			}
			fixtureSet.Append(scriptName, obs.Args, obs.Result)
			g.session.AppendCtx(ctx, session.Record{
				Call:     "tool_call",
				Provider: g.provider,
				Tool:     scriptName,
				ToolArgs: obs.Args,
				Result:   obs.Result,
			})
		}

		if err != nil {
			return nil, fixtureSet, nil, fmt.Errorf("drive tool-use loop: %w", err)
		}

		if result.Submit == nil {
			return nil, fixtureSet, nil, fmt.Errorf("model ended without calling submit_script (final text: %q)", truncate(result.FinalText, 300))
		}

		out, expected, perr := parseSubmitScript(result.Submit)
		if perr != nil {
			return nil, fixtureSet, nil, fmt.Errorf("parse submit_script: %w", perr)
		}

		diffMsg := g.verifySubmitted(ctx, n, out, expected, verifyArgs, parent, fixtureSet)
		if diffMsg == "" {
			return out, fixtureSet, expected, nil
		}
		if revision >= maxRevisions {
			return nil, fixtureSet, nil, fmt.Errorf("verification failed after %d revision: %s", revision, diffMsg)
		}
		slog.Warn("plan: verification failed, requesting revision",
			"field", n.Name,
			"diff", truncate(diffMsg, 500),
		)
		// Append a revision message — the next Drive call sees
		// the original prompt + a "you got it wrong, try again"
		// follow-up.
		messages = append(messages, llm.Message{
			Role: "user",
			Text: "Verification failed. The script you submitted, run against the data you observed during exploration, did not match your expected_output:\n\n" + diffMsg + "\n\nReview your script and your expected_output. One of them is wrong (most often the script — defensive shape handling that returns null when a wrapper key was unexpected is a common cause). Submit a corrected version.",
		})
	}

	return nil, fixtureSet, nil, fmt.Errorf("tool-use revision budget exhausted")
}

// mcpToolExecutor adapts mcpclient.Client to the llm.ToolExecutor
// interface. The tool name the model emitted may carry the SDK-side
// `github_` prefix or the CLI-side `mcp__<provider>__` prefix; both
// are stripped before calling upstream.
type mcpToolExecutor struct {
	client         *mcpclient.Client
	prefix         string // e.g. "github_"
	providerPrefix string // e.g. "mcp__atlassian__"
}

func (e *mcpToolExecutor) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	original := strings.TrimPrefix(name, e.prefix)
	original = strings.TrimPrefix(original, e.providerPrefix)
	return e.client.Call(ctx, original, args)
}

// translateObservationName maps a model-facing tool name back to the
// script-side canonical name (`github_<X>`). Both backends emit
// observations with model-facing names; fixtures use script-side
// names so verification replay matches the script's host calls.
func translateObservationName(name, provider string) string {
	if strings.HasPrefix(name, mcpclient.HostNamePrefix) {
		return name
	}
	if provider != "" {
		prefix := "mcp__" + provider + "__"
		if strings.HasPrefix(name, prefix) {
			return mcpclient.HostNamePrefix + strings.TrimPrefix(name, prefix)
		}
	}
	return mcpclient.HostNamePrefix + name
}

// verifySubmitted runs the submitted script with mocked capabilities
// (replay over fixtureSet) and deep-diffs the actual output against
// the LLM-stated expected_output. Returns "" on match (or when
// verification is skipped due to no wired runner), or a description
// of the first divergence suitable for feeding back to the LLM.
func (g *Generator) verifySubmitted(ctx context.Context, n *engine.Node, out *llmOutput, expected any, args map[string]any, parent any, set fixtures.Set) string {
	if g.monty == nil || g.caps == nil {
		slog.Info("plan: verification skipped — no runner wired", "field", n.Name)
		return ""
	}
	if violation := engine.ValidateScript(out.MontyScript); violation != "" {
		return "script failed pre-execution validation: " + violation
	}
	mockCaps := fixtures.ReplayCapabilities(g.caps, set, g.toolNames)
	mod, err := g.monty.Compile(out.MontyScript)
	if err != nil {
		return fmt.Sprintf("script failed to compile: %v", err)
	}
	start := time.Now()
	raw, _, err := g.monty.Run(ctx, mod, "execute",
		map[string]any{"args": args, "parent": parent}, mockCaps)
	rec := session.Record{
		Call:       "verify",
		Provider:   g.provider,
		Field:      n.Name,
		DurationMS: time.Since(start).Milliseconds(),
		Result:     raw,
	}
	if err != nil {
		rec.Err = err.Error()
		g.session.AppendCtx(ctx, rec)
		return fmt.Sprintf("script raised at runtime: %v", err)
	}
	diff := fixtures.Diff(expected, raw, "")
	if diff != "" {
		rec.Err = "diff: " + diff
	}
	g.session.AppendCtx(ctx, rec)
	return diff
}

// buildToolDefs assembles the Anthropic-side tool list the model can
// call during the GENERATE loop: every upstream MCP tool (with the
// monty host-name prefix, matching what the script will use) plus
// the synthetic submit_script tool.
func (g *Generator) buildToolDefs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(g.tools)+1)
	for _, t := range g.tools {
		schema := schemaToMap(t)
		defs = append(defs, llm.ToolDef{
			Name:        mcpclient.HostNamePrefix + sanitizeToolName(t.Name),
			Description: t.Description,
			InputSchema: schema,
		})
	}
	defs = append(defs, llm.ToolDef{
		Name: submitScriptToolName,
		Description: `Submit the final monty script that resolves the GraphQL node. ` +
			`Call this exactly once, when you have explored the upstream tools enough ` +
			`to write the script confidently. The engine will run the script against ` +
			`the responses you observed and verify the output matches expected_output.`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"monty_script": map[string]any{
					"type":        "string",
					"description": "The Python (Monty-on-WASM) script defining `def execute(args, parent):`.",
				},
				"expected_output": map[string]any{
					"description": "The first 1-3 elements of the answer your script will produce given the data you observed during exploration. Used to verify the script. May be a list of objects, a single object, or a scalar — match the script's return type at the top level.",
				},
				"io_schema": map[string]any{
					"description": "Best-effort JSON-Schema-ish description of the script's return shape.",
				},
			},
			"required": []any{"monty_script", "expected_output"},
		},
	})
	return defs
}

// schemaToMap reads the MCP tool's input schema as a generic
// map[string]any suitable for embedding in an Anthropic tool
// definition. Falls back to an empty object schema when the tool
// has none.
func schemaToMap(t mcp.Tool) map[string]any {
	if t.RawInputSchema != nil {
		var m map[string]any
		if err := json.Unmarshal(t.RawInputSchema, &m); err == nil {
			return m
		}
	}
	if t.InputSchema.Type != "" {
		raw, err := json.Marshal(t.InputSchema)
		if err == nil {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				return m
			}
		}
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// sanitizeToolName mirrors the same cleanup mcpclient.sanitize does
// when building monty host names. Re-implementing here (rather than
// re-exporting) keeps the import surface small; the rule is trivial.
func sanitizeToolName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// parseSubmitScript extracts the script + expected_output + io_schema
// from the submit_script tool call's input. Accepts either
// "monty_script" (the canonical key) or the "script" alias the model
// occasionally uses when paraphrasing — the prompt names the key
// explicitly but a forgiving parser is cheap insurance.
func parseSubmitScript(input map[string]any) (*llmOutput, any, error) {
	script, _ := input["monty_script"].(string)
	if script == "" {
		script, _ = input["script"].(string)
	}
	if script == "" {
		return nil, nil, errors.New("monty_script missing or not a string")
	}
	out := &llmOutput{
		MontyScript: script,
		IOSchema:    input["io_schema"],
	}
	expected, hasExpected := input["expected_output"]
	if !hasExpected {
		return nil, nil, errors.New("expected_output missing")
	}
	return out, expected, nil
}

func logUsage(call string, u llm.Usage) {
	slog.Info("plan: llm usage",
		"call", call,
		"input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens,
		"cache_create", u.CacheCreationTokens,
		"cache_read", u.CacheReadTokens,
	)
}

// llmOutput is the JSON shape we ask Claude to produce.
type llmOutput struct {
	MontyScript string `json:"monty_script"`
	IOSchema    any    `json:"io_schema,omitempty"`
}

// stripCodeFence drops a leading ```[lang]\n ... \n``` wrapper if
// present. Used by NORMALIZE response parsing where the model
// occasionally fences its JSON output.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	body := s[nl+1:]
	body = strings.TrimSuffix(body, "```")
	body = strings.TrimSuffix(body, "\n")
	return body
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// buildGenerateSystemToolUse is the system-prompt builder for the
// explore-then-submit GENERATE flow. The model is given the catalog
// as REAL Anthropic tools (added by the loop driver); it calls
// upstream MCP tools to learn the response shapes, then calls a
// synthetic submit_script tool to deliver the final script.
//
// IMPORTANT — DO NOT ADD PROVIDER-SPECIFIC TEXT. Provider knowledge
// comes from the catalog block (rendered from the live MCP tool
// list), never hardcoded here.
func buildGenerateSystemToolUse(catalog string) []llm.SystemBlock {
	preamble := `You generate Python (Monty-on-WASM) scripts that resolve a single GraphQL node against the upstream MCP server's tools (the host-function catalog below).

You work in two phases:

  1. EXPLORE. Call the upstream tools available to you (each catalog
     entry is a real callable tool in this conversation) to learn the
     response shapes. Make as many calls as you need to observe the
     data your script will navigate. Pay attention to the nesting of
     wrapper keys, the names of fields, and which IDs need a separate
     resolver call.

  2. SUBMIT. When you have enough information to write the script,
     call the synthetic ` + "`submit_script`" + ` tool with the script,
     the IO schema, and the expected first 1-3 records of the output
     the script will produce. The engine will then run your script
     against the responses you observed and verify the output matches
     your expected_output.

If the verification fails, you'll get one chance to revise.

## The script's runtime contract

The script you submit is invoked by the engine as:

    execute(args, parent)

where:
  - args   is a dict of the GraphQL field arguments at this node, with values already resolved to Python natives.
  - parent is None for top-level nodes; for nodes inside a parent's selection set, it is the parent object as the parent's script returned it (a dict, or one element of a list the parent returned).

Your script MUST define ` + "`def execute(args, parent):`" + ` and return:
  - a list of dicts when the node represents a collection,
  - a single dict when the node represents one entity,
  - a scalar (str / int / bool) when the node is a scalar leaf.

The engine handles selection projection: return whichever fields might plausibly be requested at this node — the engine drops anything not asked for in the requested selection set. Do NOT manually filter to the requested fields.

The ONLY external surface available to the script is the host-function catalog below. Do not import requests, urllib, subprocess, os, threading, asyncio, etc. Do not call open(). The Monty sandbox does not provide them.

## Project from parent when possible

If ` + "`parent`" + ` is non-None and the requested fields look like simple projections of values already present in the parent dict, return ` + "`{key: parent.get(key) for key in requested_keys}`" + ` directly. Do NOT call host functions for data the parent already supplied.

## Errors propagate (no try/except)

When a host function raises, let the exception propagate. The engine's retry loop sees the error and gives you a chance to fix the script. Wrapping a tool call in ` + "`try / except`" + ` swallows real errors and is FORBIDDEN — a static check rejects scripts containing those keywords.

Defensive shape handling on a SUCCESSFUL response is fine: ` + "`item.get(key, default)`" + `, ` + "`isinstance(x, dict)`" + `, etc.

## Sandbox Python constraints (verified)

Monty is a forked Python-on-WASM. Most of the language works, but:

ALLOWED:
  - All built-in types and operations (int, str, list, dict, tuple, set, bool, None)
  - f-strings:  f"hello {name}"  — use these for formatting
  - String methods, comprehensions, sorted(), .sort(), isinstance()
  - import json   — json.dumps, json.loads
  - import re     — re.search, re.match, re.findall, etc.
  - import datetime — but ONLY:
      * datetime.datetime(year, month, day, ...)            — constructor
      * datetime.datetime.fromisoformat("2024-01-01T...")    — parse ISO 8601
      * datetime.timedelta(days=N, hours=N, ...)
      * arithmetic on datetime/timedelta objects
      * .isoformat(), .strftime() on datetime instances

BANNED (will crash):
  - "...".format(...)        — use f-strings
  - "..." %% (a, b)          — use f-strings
  - datetime.datetime.now()  — use now_iso() / days_ago_iso(n) host helpers
  - any network, file, subprocess, or threading API

## Time host helpers

  - now_iso()             -> "2026-05-06T14:30:00Z"      RFC 3339, UTC
  - now_epoch()           -> 1746541800                  unix seconds (int)
  - days_ago_iso(n=7)     -> "2026-04-29T14:30:00Z"      n days ago, UTC
  - hours_ago_iso(n=24)   -> "2026-05-05T14:30:00Z"      n hours ago, UTC

When a query passes a numeric time arg like ` + "`days: 7`" + `, pass it directly to days_ago_iso(int(n)) — don't guard with isinstance(x, str).

## Hallucinated field names

The user may request fields that don't exist on any real schema. Treat the field name as an intent signal. If a field has no plausible mapping, return null for it; do not raise.

## Resolve opaque IDs by calling resolver tools you've explored

When the response only carries an opaque identifier (an authorId, ownerId, userId, or similar) but the user asked for a human-readable field (name, displayName, title), look in the catalog for a resolver tool — typically named ` + "`lookup_<kind>_id`" + `, ` + "`get_<kind>_by_id`" + `, etc. Call it during exploration to verify it returns what you expect, then have your script call it too. Cache the resolution within the script (one dict on the local function frame) so iterating a 50-item list doesn't make 50 round-trips.

## Parallel fan-out for independent host calls

When iterating a list and making one host call per element with NO data dependency between iterations, batch the calls with the ` + "`parallel`" + ` host helper. Sequential N×400ms calls become ~400ms total once batched.

Shape:

    results = parallel([
        {"fn": "host_function_name", "args": {"key": value_for_item_1}},
        {"fn": "host_function_name", "args": {"key": value_for_item_2}},
    ])
    # results[i] is {"ok": <return-value>} or {"error": "<msg>"}.
    # Same length and order as the input list.

Optional: ` + "`parallel(calls, max_concurrency=8)`" + ` — default 8.

Use ` + "`parallel`" + ` when:
  - Each call is independent (no call's args depend on another's result).
  - The function is a host call (an in-process Python loop has nothing to gain).
  - You're about to make at least 3 calls.

When ANY call errors, the whole batch still returns — inspect each result's "error" key. Treat a per-result error like a missing field on a single call: use a fallback or return null for that row, do NOT raise.

DO NOT use ` + "`parallel`" + ` to call itself or for pure-Python work.

## What goes in expected_output

When you call ` + "`submit_script`" + `, ` + "`expected_output`" + ` is the SHAPE the engine will compare against. Provide the FIRST 1-3 elements of what your script will return given the data you observed. The engine compares structurally — extra keys in actual output are fine, but missing keys or wrong types fail. Don't include keys you can't predict (e.g. updated timestamps if your script returns the raw upstream value); use null for fields you genuinely expect to be null.

## What goes in io_schema

Best-effort JSON-Schema-ish description of what the script returns. Used by downstream tooling for hints; not strictly enforced.
`

	return []llm.SystemBlock{
		{Text: catalog, CacheBreakAfter: true},
		{Text: preamble, CacheBreakAfter: true},
	}
}

// renderToolCatalog serialises the MCP tool list to a stable, prompt-friendly
// string. Sort by name so the output bytes are deterministic — required for
// prompt-cache hits across calls.
func renderToolCatalog(tools []mcp.Tool) string {
	var b strings.Builder
	b.WriteString("## Host function catalog\n\n")
	b.WriteString("These are the host functions available inside the monty sandbox. Call each by its `github_<name>` name with kwargs only.\n\n")

	sorted := append([]mcp.Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, t := range sorted {
		fmt.Fprintf(&b, "### github_%s\n", t.Name)
		if t.Description != "" {
			fmt.Fprintf(&b, "%s\n", strings.TrimSpace(t.Description))
		}
		if schema := renderInputSchema(t); schema != "" {
			fmt.Fprintf(&b, "Input schema (JSON Schema):\n```json\n%s\n```\n", schema)
		}
		// Output schema — when the upstream MCP server publishes one,
		// surface it. Generated scripts otherwise have to guess
		// response shapes from the response itself, which falls apart
		// when the wrapper nesting is non-obvious (e.g. data.users.users).
		// This is the catalog-driven path — no provider-specific code,
		// just letting the provider tell us about itself.
		if schema := renderOutputSchema(t); schema != "" {
			fmt.Fprintf(&b, "Output schema (JSON Schema):\n```json\n%s\n```\n", schema)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildNormalizeSystem assembles the system blocks for the NORMALIZE call.
// Same cacheable tool catalog as Generate (so we get cache hits across both
// call types), then the canonicalisation instructions.
//
// IMPORTANT — DO NOT ADD PROVIDER-SPECIFIC TEXT.
//
// Same rule as buildGenerateSystemToolUse: this prompt drives normalize for ANY
// MCP server (GitHub, Atlassian, Linear, Slack, …). Provider-specific
// canonical names come from the runtime-rendered tool catalog (the
// first block); never hardcode mappings like "openPRs → pull_requests"
// here. Examples below use ABSTRACT patterns to illustrate the rules.
//
// CacheBreakAfter on BOTH blocks so the prompt cache hits past the
// preamble too — saves ~10k tokens/call after the first.
func buildNormalizeSystem(catalog string) []llm.SystemBlock {
	preamble := `You receive a GraphQL node — a (possibly hallucinated) field name, its argument names, and its recursive selection — and produce a canonical, normalised representation of the same intent.

Two different surface phrasings with the same meaning MUST produce the same canonical schema. This is the LOAD-BEARING contract — getting it wrong fragments the cache and wastes a generation. Be aggressive about collapsing paraphrases.

## How to choose canonical names

Use the tool catalog above as your source of truth. The catalog lists the actual tool names and input/output schemas the upstream MCP server exposes. Canonical names should track those, not the user's surface form.

Rules:

  - Field names: snake_case, lowercase.
  - Top-level fields: prefer the underlying tool's primary entity name (e.g. the noun in the tool name, stripped of "list_", "get_", "search_" prefixes). If multiple tools could plausibly serve the request, pick the one whose schema best matches the user's args + selection.
  - Nested selection fields: prefer the field name the underlying tool's response actually returns (visible in the tool's output schema). The user's literal name goes in field_rename.
  - Args: prefer the underlying tool's parameter name. The user's literal goes in arg_rename.

When in doubt, prefer the tool catalog's primary input/output name over the user's surface form.

## Do NOT collapse semantically-distinct args

Two args that pick different facets of the same entity are NOT the same. Keep them distinct in the canonical schema even if their canonical names differ from the user's literals. Examples of distinctions worth preserving (general patterns; check the actual tool catalog for which apply):

  - "wrote it" vs "applied it" vs "approved it"
  - "owns the work" vs "approves the work"
  - "source" vs "target" of a relation
  - opposite directions of a relation
  - orthogonal filters that combine

The script-side handles arg VALUES at runtime, but arg NAMES with different semantics drive different code paths and therefore different cached scripts. Preserve them.

## Never silently drop a user-supplied arg

Every arg the user wrote at this node MUST appear in ` + "`canonical_schema.args`" + ` (under its canonical name) AND in ` + "`arg_rename`" + ` (mapping the literal to the canonical). Dropping an arg from canonical_schema means GENERATE never sees it — the user's filter, limit, or sort silently disappears and the result set is wrong without any error surface.

If you believe the arg has no plausible counterpart in any catalog tool's input schema, still keep it in canonical_schema.args under its closest sensible canonical name (e.g. ` + "`limit`" + `, ` + "`max_results`" + `, ` + "`page_size`" + ` for size-bounding args; ` + "`since`" + `, ` + "`updated_after`" + ` for time-window args). The script side decides what to do with it; NORMALIZE's job is to preserve user intent.

Bad:
  Input:  { thingies(text: "foo", limit: 5) { ... } }
  Output: canonical_schema.args = ["query"]            ← lost limit
          arg_rename = {"text": "query", "limit": "limit"}   ← rename present but excluded from args

Good:
  Output: canonical_schema.args = ["limit", "query"]   ← both preserved, alphabetical
          arg_rename = {"text": "query", "limit": "limit"}

## Implied-arg rule (paraphrases that elide a default)

When the user's surface form implies an arg via the field name itself (e.g. a name like "open_X" implies a state=open filter), include that arg name in the canonical_schema's args list. This is what makes paraphrases collide:

  Input:  { open_X(owner: "x", repo: "y") { ... } }
  Implies: state="open"
  Output: canonical_schema.args = ["owner", "repo", "state"]
          arg_rename = {"owner": "owner", "repo": "repo"}    # state isn't user-supplied here, so no entry

  The script can default args.get("state", "open") at runtime; the engine doesn't need to inject a value. The point is the canonical args LIST matches the underlying tool's args, so paraphrases collide.

## Worked examples (illustrative — apply the same rules to whatever tools are in the catalog)

These examples use abstract names. The point is the SHAPE of the rename, not the specific mappings.

Input:  { thingies(filter_a: "x", filter_b: "y") { title num } }
        (Catalog has tool "list_things" returning items with "title" and "number" fields.)
Output: canonical_schema = { field: "things", args: ["filter_a", "filter_b"], selection: [{field: "number"}, {field: "title"}] }
        arg_rename   = {"filter_a": "filter_a", "filter_b": "filter_b"}
        field_rename = {"title": "title", "num": "number"}

Input:  { thing(by_alias: "abc", display_name: "y") { description count } }
        (Catalog has tool "get_thing" taking "id" and "name", returning "description" and "count".)
Output: canonical_schema = { field: "thing", args: ["id", "name"], selection: [{field: "count"}, {field: "description"}] }
        arg_rename   = {"by_alias": "id", "display_name": "name"}
        field_rename = {"description": "description", "count": "count"}

Input:  { author { username } }
        (Catalog tool returns user objects whose primary identifier is "login".)
Output: canonical_schema = { field: "user", args: [], selection: [{field: "login"}] }
        arg_rename   = {}
        field_rename = {"username": "login"}

## Output rules — these are LOAD-BEARING for the cache key

  - Field names: snake_case, lowercase. Use names derived from the tool catalog.
  - Args: alphabetical order, deduplicated, no values.
  - Selection: alphabetical order by canonical field name.
  - Scalar leaves get empty args and empty selection.
  - DO NOT include arg values in the canonical_schema.
  - DO NOT keep paraphrase-specific surface names in the canonical_schema; map them. The rename maps preserve the literal names so the engine can translate around the cached script.

Respond with a single JSON object — no prose, no markdown fence:

    {
      "canonical_schema": {
        "field": "...",
        "args": ["..."],
        "selection": [
          {"field": "...", "args": [], "selection": []}
        ]
      },
      "arg_rename": {
        "<literal_arg_name>": "<canonical_arg_name>"
      },
      "field_rename": {
        "<literal_child_field_name>": "<canonical_child_field_name>"
      }
    }

` + "`arg_rename`" + ` covers ONLY the args at THIS top-level node. ` + "`field_rename`" + ` covers ONLY the direct children of THIS node (not deeper levels — children are normalised on their own descent). Both maps are required even if every entry is identity (literal == canonical) — emit identity entries explicitly.
`
	return []llm.SystemBlock{
		{Text: catalog, CacheBreakAfter: true},
		{Text: preamble, CacheBreakAfter: true},
	}
}

// renderInputSchema serialises a tool's input schema to a stable, indented
// JSON string for the prompt. Returns empty if no schema is set.
func renderInputSchema(t mcp.Tool) string {
	if t.RawInputSchema != nil {
		var v any
		if err := json.Unmarshal(t.RawInputSchema, &v); err == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		}
		return string(t.RawInputSchema)
	}
	if t.InputSchema.Type != "" {
		b, err := json.MarshalIndent(t.InputSchema, "", "  ")
		if err == nil {
			return string(b)
		}
	}
	return ""
}

// renderOutputSchema serialises a tool's output schema (the response shape
// the upstream tool returns) to JSON. Optional under MCP — many servers
// don't publish one; returns empty in that case and the script has to
// infer the shape from runtime probing.
func renderOutputSchema(t mcp.Tool) string {
	if t.RawOutputSchema != nil {
		var v any
		if err := json.Unmarshal(t.RawOutputSchema, &v); err == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		}
		return string(t.RawOutputSchema)
	}
	if t.OutputSchema.Type != "" {
		b, err := json.MarshalIndent(t.OutputSchema, "", "  ")
		if err == nil {
			return string(b)
		}
	}
	return ""
}

// buildUserPrompt renders the per-call user message for the GENERATE call.
// `canon` (from the preceding normalize call) is what fixes the I/O contract
// for the cached script — args and output keys must be the canonical names,
// not the literal user names, so future paraphrases can reuse the script
// via the L2 cache with the engine's rename layer translating around it.
func buildUserPrompt(n *engine.Node, parent any, canon *canonicalSchema) (string, error) {
	shape := n.Shape()
	shapeJSON, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		return "", err
	}

	canonJSON := []byte("(no canonical schema available)")
	if canon != nil {
		canonJSON, err = json.MarshalIndent(canon, "", "  ")
		if err != nil {
			return "", err
		}
	}

	parentBlock := "(none — this is a top-level node)"
	if parent != nil {
		parentJSON, err := json.MarshalIndent(parent, "", "  ")
		if err == nil {
			s := string(parentJSON)
			if len(s) > 4000 {
				s = s[:4000] + "\n  ...truncated..."
			}
			parentBlock = s
		}
	}

	return fmt.Sprintf(`Generate the monty script for this node.

The script's runtime I/O contract is defined by the CANONICAL schema, not the literal user phrasing — the engine renames around your script so other paraphrases can reuse it via the L2 cache. Use the canonical names everywhere in the script.

Literal user node (for context only):
%s

Canonical schema — USE THESE NAMES inside the script (args["..."] keys + the keys of the dicts you return must be these canonical names):
%s

Parent context (already canonical-keyed; receive it as `+"`parent`"+`):
%s

Now produce the JSON response.`, shapeJSON, canonJSON, parentBlock), nil
}
