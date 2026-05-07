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
	"github.com/sleuth-io/genie/internal/llm"
	"github.com/sleuth-io/genie/internal/mcpclient"
	"github.com/sleuth-io/genie/internal/progress"
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
	client          llm.Client
	store           *crystallize.Store
	generateSystem  []llm.SystemBlock
	normalizeSystem []llm.SystemBlock
	toolNames       []string // monty-side names like github_list_pull_requests
	tools           []mcp.Tool
	provider        string // routing key — recorded in session entries
	session         *session.Session
	normalizeModel  string // GENIE_NORMALIZE_MODEL — empty ⇒ backend default
	generateModel   string // GENIE_GENERATE_MODEL — empty ⇒ backend default

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
		client:          llmClient,
		store:           store,
		generateSystem:  buildGenerateSystem(catalog),
		normalizeSystem: buildNormalizeSystem(catalog),
		toolNames:       c.MontyToolNames(),
		tools:           tools,
		provider:        provider,
		session:         sess,
		normalizeModel:  os.Getenv("GENIE_NORMALIZE_MODEL"),
		generateModel:   os.Getenv("GENIE_GENERATE_MODEL"),
	}
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

// Regenerate implements engine.ScriptRetryer. Called by the executor
// when a previously-generated script errors at compile or run time
// (typically a tool-call returning an upstream constraint violation
// — e.g. Atlassian's "Unbounded JQL queries are not allowed here").
//
// We re-run the GENERATE prompt with the failed script and the
// verbatim error appended as user-message context so the LLM can
// adjust. The new script replaces the L2 cache entry — future calls
// against this canonical hash get the fixed version directly without
// paying for another retry.
//
// One retry per node (the executor caps at one). If the regeneration
// itself fails or the new script also errors, we surface the
// original error.
func (g *Generator) Regenerate(ctx context.Context, n *engine.Node, parent any, prevScript, prevErr string) (string, *engine.NodeRename, error) {
	// Reload state for this node — we need the canonical schema and
	// rename that NORMALIZE produced earlier. They're already in
	// the L2 cache (we wrote them on the first attempt).
	shape := n.Shape()
	literalHash := shape.L1Hash()
	alias, ok, err := g.store.GetAlias(literalHash)
	if err != nil || !ok {
		return "", nil, fmt.Errorf("regenerate: no alias for shape %s (was the first attempt persisted?)", literalHash[:12])
	}
	entry, ok, err := g.store.GetEntry(alias.CanonicalHash)
	if err != nil || !ok {
		return "", nil, fmt.Errorf("regenerate: no L2 entry for canonical %s", alias.CanonicalHash[:12])
	}

	canon := canonicalSchema{}
	if entry.CanonicalSchema != nil {
		_ = json.Unmarshal(entry.CanonicalSchema, &canon)
	}

	userText, err := buildRetryPrompt(n, parent, &canon, prevScript, prevErr)
	if err != nil {
		return "", nil, fmt.Errorf("build retry prompt: %w", err)
	}

	slog.Info("plan: regenerating after script error",
		"field", n.Name,
		"canonical_hash", alias.CanonicalHash[:12],
		"prev_err", truncate(prevErr, 200))

	resp, err := g.callLLM(ctx, "regenerate", n.Name, g.generateSystem, userText)
	if err != nil {
		return "", nil, fmt.Errorf("regenerate llm call: %w", err)
	}
	out, err := parseLLMResponse(resp.Text)
	if err != nil {
		return "", nil, fmt.Errorf("parse regenerate response: %w", err)
	}
	g.metrics.GenerateCalls++
	g.metrics.GenerateInputTokens += resp.Usage.InputTokens
	g.metrics.GenerateOutputTokens += resp.Usage.OutputTokens

	// Replace the L2 entry so subsequent calls skip the broken
	// version. Alias unchanged — same literal-shape → same
	// canonical hash.
	entry.MontyScript = out.MontyScript
	entry.IOSchema = out.IOSchema
	if err := g.store.PutEntry(*entry); err != nil {
		slog.Warn("plan: regenerated entry write failed", "err", err)
	}
	return out.MontyScript, alias.Rename, nil
}

// buildRetryPrompt extends the standard GENERATE user prompt with
// the previous attempt's script and error. The LLM sees the original
// task plus what just went wrong; the GENERATE system prompt's
// instructions about output shape stay in force.
func buildRetryPrompt(n *engine.Node, parent any, canon *canonicalSchema, prevScript, prevErr string) (string, error) {
	original, err := buildUserPrompt(n, parent, canon)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(original)
	b.WriteString("\n\n## Previous attempt failed\n\n")
	b.WriteString("Your earlier script:\n\n```python\n")
	b.WriteString(prevScript)
	b.WriteString("\n```\n\nFailed with:\n\n```\n")
	b.WriteString(prevErr)
	b.WriteString("\n```\n\n")
	b.WriteString("The error message is verbatim from the upstream system; trust the constraint it describes. Generate a corrected script that respects it. Output the same JSON shape (monty_script + io_schema) as before.\n")
	return b.String(), nil
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
	out, err := g.fullGenerate(ctx, n, parent, canonSchema)
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

// fullGenerate runs the GENERATE LLM call and parses the JSON response.
// canon (from the prior normalize call) tells the LLM which canonical names
// to use in the script's I/O — so any future L2 hit can reuse the script
// with stable canonical-keyed args and outputs.
func (g *Generator) fullGenerate(ctx context.Context, n *engine.Node, parent any, canon *canonicalSchema) (*llmOutput, error) {
	userText, err := buildUserPrompt(n, parent, canon)
	if err != nil {
		return nil, fmt.Errorf("build user prompt: %w", err)
	}

	slog.Info("plan: full generate",
		"field", n.Name,
		"shape_hash", n.Shape().L1Hash()[:12],
	)

	resp, err := g.callLLM(ctx, "generate", n.Name, g.generateSystem, userText)
	if err != nil {
		return nil, fmt.Errorf("llm call: %w", err)
	}
	logUsage("generate", resp.Usage)
	g.metrics.GenerateCalls++
	g.metrics.GenerateInputTokens += resp.Usage.InputTokens
	g.metrics.GenerateOutputTokens += resp.Usage.OutputTokens
	g.metrics.CacheReadInputTokens += resp.Usage.CacheReadTokens
	g.metrics.CacheCreationInputTokens += resp.Usage.CacheCreationTokens

	out, err := parseLLMResponse(resp.Text)
	if err != nil {
		return nil, fmt.Errorf("parse llm response: %w (raw=%q)", err, truncate(resp.Text, 500))
	}

	// Pre-persist validation loop. The executor also runs
	// ValidateScript (executor.go:resolveNode) before executing —
	// but by then the bad script is already in L2. If we let it
	// land, every paraphrase that L2-hits this canonical schema
	// will pay an avoidable retry round-trip. Catch it here and
	// regenerate with the violation as feedback so only a
	// compliant script reaches PutEntry.
	for attempt := 0; attempt < engine.RetryLimit(); attempt++ {
		violation := engine.ValidateScript(out.MontyScript)
		if violation == "" {
			break
		}
		slog.Warn("plan: generated script violates static invariant; regenerating",
			"field", n.Name,
			"violation", violation,
			"attempt", attempt+1,
		)
		retryText, berr := buildRetryPrompt(n, parent, canon, out.MontyScript, violation)
		if berr != nil {
			return nil, fmt.Errorf("build validation-retry prompt: %w", berr)
		}
		resp, err := g.callLLM(ctx, "regenerate", n.Name, g.generateSystem, retryText)
		if err != nil {
			return nil, fmt.Errorf("validation-retry llm call: %w", err)
		}
		logUsage("regenerate", resp.Usage)
		g.metrics.GenerateCalls++
		g.metrics.GenerateInputTokens += resp.Usage.InputTokens
		g.metrics.GenerateOutputTokens += resp.Usage.OutputTokens
		g.metrics.CacheReadInputTokens += resp.Usage.CacheReadTokens
		g.metrics.CacheCreationInputTokens += resp.Usage.CacheCreationTokens
		next, perr := parseLLMResponse(resp.Text)
		if perr != nil {
			return nil, fmt.Errorf("parse validation-retry response: %w", perr)
		}
		out = next
	}
	if violation := engine.ValidateScript(out.MontyScript); violation != "" {
		return nil, fmt.Errorf("script validation failed after %d attempts: %s", engine.RetryLimit(), violation)
	}

	return out, nil
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

// parseLLMResponse extracts the JSON object from Claude's text response.
// We instruct the model to return JSON only, but the SDK still returns
// plain text — so we parse defensively, allowing for an optional code-fence
// wrapper (```json ... ```).
//
// On strict-parse failure, retry with literal control chars inside string
// values escaped. The model occasionally emits a real newline/CR/tab inside
// the monty_script string value (technically invalid JSON per RFC 8259 §7,
// but a frequent enough output mode that the retry path can't afford to
// die on it — losing the regenerate response throws away a good script).
func parseLLMResponse(text string) (*llmOutput, error) {
	body := strings.TrimSpace(text)
	body = stripCodeFence(body)

	var out llmOutput
	err := json.Unmarshal([]byte(body), &out)
	if err != nil {
		fixed := escapeUnescapedControlsInJSONStrings(body)
		err = json.Unmarshal([]byte(fixed), &out)
		if err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
	}
	if out.MontyScript == "" {
		return nil, errors.New("missing monty_script field")
	}
	return &out, nil
}

// escapeUnescapedControlsInJSONStrings walks the JSON body and replaces
// any literal newline / carriage return / tab that appears INSIDE a string
// value with its proper JSON escape. Outside string values these characters
// are legal whitespace and are passed through unchanged.
//
// The walk tracks string-boundary state via a single boolean and respects
// backslash escapes — when a backslash appears inside a string, the next
// character is consumed verbatim so an already-escaped quote doesn't
// confuse the parser.
//
// Bytes outside the ASCII range are passed through unchanged; the JSON
// spec only flags chars < 0x20 inside strings, and runes are rebuilt at
// unmarshal time.
func escapeUnescapedControlsInJSONStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	inString := false
	escapeNext := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			continue
		}
		if escapeNext {
			b.WriteByte(c)
			escapeNext = false
			continue
		}
		switch c {
		case '\\':
			escapeNext = true
			b.WriteByte(c)
		case '"':
			inString = false
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop opening fence (with optional language tag) and trailing fence.
	// Format: ```[lang]\n<body>\n```
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

// buildGenerateSystem assembles the system blocks for the GENERATE call:
// the cacheable tool catalog (cache breakpoint) followed by the script-
// generation instructions. The tool catalog renders deterministically so
// the prompt cache hits across calls.
//
// IMPORTANT — DO NOT ADD PROVIDER-SPECIFIC TEXT.
//
// Genie is a generic MCP gateway. The exact same prompt is used to drive
// scripts against GitHub, Atlassian, Linear, Slack, or any other MCP
// server the user wires up. Provider-specific knowledge (tool names,
// schema fields, response shapes) MUST come from the runtime-rendered
// tool catalog (the first block) — never from hardcoded prose here.
//
// Examples in this prompt use abstract patterns (e.g. "items / records /
// data" rather than specific field names like "pull_requests"). If you
// catch yourself writing a specific tool name or schema field into the
// preamble, stop — that knowledge belongs in the catalog block, which
// comes from the upstream MCP server's actual tool list.
//
// CacheBreakAfter on BOTH blocks: the catalog and preamble are stable
// across calls, so caching both saves ~12k tokens/call after the first.
func buildGenerateSystem(catalog string) []llm.SystemBlock {
	preamble := `You generate Python (Monty-on-WASM) scripts that resolve a single GraphQL node against the upstream MCP server's tools (the host-function catalog below).

The script you produce is invoked by the engine as:

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

## Project from parent when possible (avoid redundant tool calls)

If ` + "`parent`" + ` is non-None and the requested fields look like simple projections of values already present in the parent dict, return ` + "`{key: parent.get(key) for key in requested_keys}`" + ` directly. Do NOT call host functions for data the parent already supplied. The engine reuses the parent script's output across all child nodes, so re-fetching is pure waste.

## Do NOT swallow host-function errors

When a host function raises (an upstream tool returned an error response), let the exception propagate. The executor catches it and gives you a chance to regenerate the script with the verbatim error in your next prompt — that's how you learn about constraints like "this query needs a filter argument" or "this field requires expand=X".

DO NOT write:

    try:
        resp = some_tool(...)
    except Exception:
        resp = None   # ← swallows real errors, breaks the retry loop

DO write:

    resp = some_tool(...)   # let upstream errors propagate

(Defensive shape handling on the SUCCESSFUL response is fine; see below.)

## Sandbox Python constraints (verified)

Monty is a forked Python-on-WASM. Most of the language works, but the following diverges from CPython. You MUST follow these rules — scripts that violate them crash at runtime and the cache entry is wasted.

ALLOWED (verified working):
  - All built-in types and operations (int, str, list, dict, tuple, set, bool, None)
  - f-strings:  f"hello {name}"  — use these for formatting
  - String methods: split, join, strip, lower, upper, replace, startswith, endswith, find, in, slicing
  - List/dict/set comprehensions, sorted(), .sort(), isinstance()
  - import json     — json.dumps, json.loads
  - import re       — re.search, re.match, re.findall, etc.
  - import datetime — but ONLY:
      * datetime.datetime(year, month, day, ...)            — constructor
      * datetime.datetime.fromisoformat("2024-01-01T...")    — parse ISO 8601
      * datetime.timedelta(days=N, hours=N, ...)
      * arithmetic on datetime/timedelta objects
      * .isoformat(), .strftime() on datetime instances

BANNED (will crash):
  - "...".format(...)        — use f-strings instead
  - "..." %% (a, b)          — use f-strings instead
  - datetime.datetime.now()  — use the host helpers below
  - datetime.datetime.utcnow()
  - datetime.date.today()
  - any network, file, subprocess, or threading API

## Time host helpers

For "current time" needs, call these host functions instead of any datetime
clock API. They're regular host calls — no import needed.

  - now_iso()             -> "2026-05-06T14:30:00Z"      RFC 3339, UTC
  - now_epoch()           -> 1746541800                  unix seconds (int)
  - days_ago_iso(n=7)     -> "2026-04-29T14:30:00Z"      RFC 3339, UTC, n days ago
  - hours_ago_iso(n=24)   -> "2026-05-05T14:30:00Z"      RFC 3339, UTC, n hours ago

When a query passes a numeric time arg like ` + "`days: 7`" + ` or ` + "`hours: 24`" + `,
the value reaches your script as a Python int. Pass it directly:

    n = args.get("days") or 7
    since = days_ago_iso(n)            # works whether n is 7 (int) or 7.0 (float)

Do NOT guard with ` + "`isinstance(x, str)`" + ` and skip the helper when the
arg is numeric — the check fails for ints, the time clause silently
drops, and the user's filter is ignored. Coerce explicitly if you
need to:

    n = args.get("days")
    if isinstance(n, str):
        n = int(n) if n.isdigit() else 7
    elif not isinstance(n, (int, float)):
        n = 7
    since = days_ago_iso(int(n))

## Hallucinated field names

Hallucinated GraphQL field names are intentional: the user may request fields that do not exist on any real schema. Treat the field name as an intent signal — pick the closest sensible interpretation given the available tools and the arg list. If a requested field has no plausible mapping, return null for it; do not raise.

## Resolve opaque identifiers when a human-readable field is requested

The user's selection often asks for human-readable fields (` + "`name`" + `, ` + "`displayName`" + `, ` + "`title`" + `, ` + "`label`" + `) at a position where the upstream tool's response only carries an opaque identifier (e.g. ` + "`authorId`" + `, ` + "`ownerId`" + `, ` + "`userId`" + `, ` + "`assigneeAccountId`" + `, ` + "`createdById`" + `). Returning the raw ID where a name was requested is a silent data bug — the caller can't tell, and the answer is unusable.

When this happens, scan the catalog for a tool that resolves the ID kind to a record, and call it. Common naming patterns: ` + "`lookup_<kind>_id`" + `, ` + "`get_<kind>_by_id`" + `, ` + "`get_<kind>`" + `. Pick the one whose input schema accepts the opaque ID you have and whose output schema contains the human-readable field you need.

Pseudocode:

    raw_value = item.get("displayName") or item.get("name")
    if raw_value and is_opaque_id(raw_value):
        # The upstream returned just an ID; look up the record to get
        # the actual name. The catalog has a tool for this — use it.
        resolved = lookup_kind_by_id(id=raw_value)
        if isinstance(resolved, dict):
            raw_value = resolved.get("displayName") or resolved.get("name") or raw_value

` + "`is_opaque_id(s)`" + ` is up to your judgement: typically a hex/uuid-shaped string, an all-digit string, or any value that obviously isn't a human name. When in doubt, attempt the lookup — if the catalog has no resolver tool, return the raw value as-is rather than raising.

Cache the resolution within the script (one dict on the local function frame) so iterating a 50-item list doesn't make 50 round-trips for the same ID.

## Defensive shape handling on SUCCESSFUL responses

A cached script may be reused for a paraphrase where the response shape differs subtly. On a successful response (the call did not raise), defensively handle every plausible shape rather than assuming one.

  - When you expect a list, accept any of:
      * a bare ` + "`list`" + ` of dicts
      * a ` + "`dict`" + ` whose value at one of common wrapper keys ({"items", "results", "data", "nodes", "values", "records"}, plus any wrapper key the actual tool's schema names) is the list
      * an empty/None response
    Pseudocode:
        if isinstance(resp, list):
            items = resp
        elif isinstance(resp, dict):
            items = resp.get("items") or resp.get("results") or resp.get("data") or resp.get("nodes") or resp.get("values") or resp.get("records") or []
        else:
            items = []

  - When iterating the resolved list, SKIP non-dict elements:
        for it in items:
            if not isinstance(it, dict):
                continue
            ...

  - When projecting nested objects, default to ` + "`{}`" + ` and use ` + "`.get()`" + `:
        sub = (item.get("sub_obj") or {})
        val = sub.get("name")

  - On a single-object query, accept either a bare dict or a {"data": {...}} wrapper:
        if isinstance(resp, dict) and "data" in resp and isinstance(resp["data"], dict):
            obj = resp["data"]
        else:
            obj = resp if isinstance(resp, dict) else {}

Never assume a key is present. Never assume a value is the type you expect. The engine treats a None at a missing field as "not requested" and that's fine.

## Expand parameters

Many tools return shallow records by default and require an explicit ` + "`expand=`" + `, ` + "`fields=`" + `, or ` + "`include=`" + ` argument to populate nested objects. If the tool's schema (above) lists fields under ` + "`_expandable`" + ` or notes that they require expansion, set the appropriate parameter rather than relying on ` + "`.get()`" + ` returning the value silently.

## Output format

Respond with a single JSON object, no prose, no markdown fence:

    {
      "monty_script": "def execute(args, parent):\n    ...",
      "io_schema":    { "type": "...", ... }   // best-effort JSON-schema-ish description of what the script returns
    }
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
		if schema := renderSchema(t); schema != "" {
			fmt.Fprintf(&b, "Input schema (JSON Schema):\n```json\n%s\n```\n", schema)
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
// Same rule as buildGenerateSystem: this prompt drives normalize for ANY
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

// renderSchema serialises a tool's input schema to a stable, indented JSON
// string for the prompt. Returns empty if no schema is set.
func renderSchema(t mcp.Tool) string {
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
