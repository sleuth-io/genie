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
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sleuth-io/genie/internal/crystallize"
	"github.com/sleuth-io/genie/internal/engine"
	"github.com/sleuth-io/genie/internal/llm"
	"github.com/sleuth-io/genie/internal/mcpclient"
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
	}
}

// callLLM is a thin wrapper around g.client.Generate that records
// the request/response pair to the JSONL session log. callType is
// "normalize" or "generate"; field is the GraphQL field name being
// resolved (for context when reading the log).
func (g *Generator) callLLM(ctx context.Context, callType, field string, system []llm.SystemBlock, userText string) (llm.Response, error) {
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
func parseLLMResponse(text string) (*llmOutput, error) {
	body := strings.TrimSpace(text)
	body = stripCodeFence(body)

	var out llmOutput
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if out.MontyScript == "" {
		return nil, errors.New("missing monty_script field")
	}
	return &out, nil
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
func buildGenerateSystem(catalog string) []llm.SystemBlock {
	preamble := `You generate Python (Monty-on-WASM) scripts that resolve a single GraphQL node against the GitHub API via MCP host functions.

The script you produce is invoked by the engine as:

    execute(args, parent)

where:
  - args   is a dict of the GraphQL field arguments at this node, with values already resolved to Python natives.
  - parent is None for top-level nodes; for nodes inside a parent's selection set, it is the parent object as the parent's script returned it (a dict, or one element of a list the parent returned).

Your script MUST define ` + "`def execute(args, parent):`" + ` and return:
  - a list of dicts when the node is plural (e.g. pull_requests, issues),
  - a single dict when the node is singular (e.g. viewer, repository),
  - a scalar (str / int / bool) when the node is a scalar leaf.

The engine handles selection projection: return whichever fields might plausibly be requested at this node — the engine drops anything not asked for in the requested selection set. Do NOT manually filter to the requested fields.

The ONLY external surface available to the script is the host-function catalog below. Do not import requests, urllib, subprocess, os, threading, asyncio, etc. Do not call open(). The Monty sandbox does not provide them.

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

Typical usage for "X in the last 7 days":

    since = days_ago_iso(n=7)
    items = github_list_pull_requests(owner=..., repo=..., state="open")
    items = [pr for pr in items if pr.get("created_at", "") >= since]

## Hallucinated field names

Hallucinated GraphQL field names are intentional: the user may request fields that do not exist on any real schema. Treat the field name as an intent signal — pick the closest sensible interpretation given the available tools and the arg list. If a requested field has no plausible mapping, return null for it; do not raise.

## Defensive shape handling (REQUIRED — cached scripts are reused across paraphrases)

GitHub MCP tool responses are not always plain lists. A cached script may be reused for a paraphrase where the response shape differs subtly. Your script MUST defensively handle every plausible response shape rather than assuming one. Cached scripts that crash on shape variation poison the cache for every future paraphrase that L2-hits them.

Concretely:

  - When you expect a list, accept any of:
      * a bare ` + "`list`" + ` of dicts
      * a ` + "`dict`" + ` whose value at one of {"items", "issues", "pull_requests", "commits", "repositories", "results", "data", "nodes"} is the list
      * an empty/None response
    Pseudocode:
        if isinstance(resp, list):
            items = resp
        elif isinstance(resp, dict):
            items = resp.get("items") or resp.get("issues") or resp.get("pull_requests") or resp.get("commits") or resp.get("repositories") or resp.get("results") or resp.get("data") or resp.get("nodes") or []
        else:
            items = []

  - When iterating the resolved list, SKIP non-dict elements:
        for it in items:
            if not isinstance(it, dict):
                continue
            ...

  - When projecting nested objects, default to ` + "`{}`" + ` and use ` + "`.get()`" + `:
        user = (pr.get("user") or {})
        login = user.get("login")

  - On a single-object query (e.g. viewer, repository), accept either a bare dict or a wrapper:
        if isinstance(resp, dict) and "data" in resp and isinstance(resp["data"], dict):
            obj = resp["data"]
        else:
            obj = resp if isinstance(resp, dict) else {}

Never assume a key is present. Never assume a value is the type you expect. The engine treats a None at a missing field as "not requested" and that's fine.

## Output format

Respond with a single JSON object, no prose, no markdown fence:

    {
      "monty_script": "def execute(args, parent):\n    ...",
      "io_schema":    { "type": "...", ... }   // best-effort JSON-schema-ish description of what the script returns
    }
`

	return []llm.SystemBlock{
		{Text: catalog, CacheBreakAfter: true},
		{Text: preamble},
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
func buildNormalizeSystem(catalog string) []llm.SystemBlock {
	preamble := `You receive a GraphQL node — a (possibly hallucinated) field name, its argument names, and its recursive selection — and produce a canonical, normalised representation of the same intent.

Two different surface phrasings with the same meaning MUST produce the same canonical schema. This is the LOAD-BEARING contract — getting it wrong fragments the cache and wastes a generation. Be aggressive about collapsing paraphrases.

## Canonical name table (memorise these mappings)

Top-level fields — always map to the snake_case form (typically the GitHub MCP tool name without the leading "list_" or "get_" prefix):

  openPRs, pullRequests, prs, pull_request_list  → "pull_requests"
  openIssues, issues_list, issueList, issue_list → "issues"
  recent_commits, commitHistory, commits         → "commits"
  myProfile, currentUser, viewer, me             → "viewer"
  repo, repository, repository_info               → "repository"
  search                                          → keep "search_repositories" / "search_issues" / etc. based on the searched type

Nested selection fields:

  username                  → "login"
  user, owner.user          → "user"   (object) — its login lives at .login
  hash                      → "sha"
  description, summary      → "body" or "description" — pick what the underlying tool returns; default "body" for issues/PRs, "description" for repos
  star_count, stargazers    → "stargazers_count"
  fork_count                → "forks_count"
  num_reviewers, reviewer_count → "reviewer_count" (synthesised; doc "from reviewRequests/reviews count")
  days_open, time_open      → "days_open"           (synthesised; doc as "derived from created_at + now")

Args:

  name (when on a single repo) → "repo"
  q, search_query, term         → "query"
  since, after_date              → "since"
  state values "OPEN"/"CLOSED" / "open"/"closed" — keep canonical name "state"; pick a single canonical value form (lowercase) only when the value is part of the schema (don't move state values into the canonical schema; the rename handles it).

When in doubt, prefer the underlying tool's primary input/output field name over the user's surface form.

## Do NOT collapse semantically-distinct args

Two args that pick different facets of the same entity are NOT the same. Keep them distinct in the canonical schema:

  author      ≠ committer        (commits — wrote vs applied)
  author      ≠ reviewer         (PRs — wrote vs reviews)
  reviewer    ≠ assignee         (PRs — gates merge vs owns work)
  base        ≠ head             (PRs — target vs source branch)
  followers   ≠ following        (users — opposite direction)
  labels      ≠ milestone        (issues — orthogonal filters)

The script-side handles arg VALUES at runtime, but arg NAMES with different semantics drive different code paths and therefore different cached scripts. Preserve them.

## Implied-arg rule (paraphrases that elide a default)

When the user's surface form implies an arg via the field name itself (e.g. "openPRs" implies state=open, "recentCommits" implies a default branch), include that arg name in the canonical_schema's args list and surface it in arg_rename. This is what makes "openPRs" collide with "pull_requests(state: 'open')":

  Input:  { openPRs(owner: "x", repo: "y") { title number } }
  Output: canonical_schema.args = ["owner", "repo", "state"]
          arg_rename = {"owner": "owner", "repo": "repo"}    # state isn't user-supplied here, so no entry

  The script can default args.get("state", "open") at runtime; the engine doesn't need to inject a value. The point is the canonical args LIST matches the underlying tool's args, so paraphrases collide.

## Worked examples

Input:  { openIssues(owner: "x", repo: "y") { title num } }
Output: canonical_schema = { field: "issues", args: ["owner", "repo"], selection: [{field: "number"}, {field: "title"}] }
        arg_rename   = {"owner": "owner", "repo": "repo"}
        field_rename = {"title": "title", "num": "number"}

Input:  { repo(owner: "x", name: "y") { description star_count } }
Output: canonical_schema = { field: "repository", args: ["owner", "repo"], selection: [{field: "description"}, {field: "stargazers_count"}] }
        arg_rename   = {"owner": "owner", "name": "repo"}
        field_rename = {"description": "description", "star_count": "stargazers_count"}

Input:  { author { username } }
Output: canonical_schema = { field: "user", args: [], selection: [{field: "login"}] }
        arg_rename   = {}
        field_rename = {"username": "login"}

## Output rules — these are LOAD-BEARING for the cache key

  - Field names: snake_case, lowercase. Use the canonical-table mappings above.
  - Args: alphabetical order, deduplicated, no values.
  - Selection: alphabetical order by canonical field name.
  - Scalar leaves get empty args and empty selection.
  - DO NOT include arg values in the canonical_schema.
  - DO NOT keep paraphrase-specific names ("openIssues", "openPRs") in the canonical_schema; map them. The rename maps preserve the literal names so the engine can translate around the cached script.

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

Worked example for input ` + "`{ openPRs(name: \"sdk\") { title num }`" + `:

    {
      "canonical_schema": {
        "field": "pull_requests",
        "args": ["repo"],
        "selection": [
          {"field": "number", "args": [], "selection": []},
          {"field": "title", "args": [], "selection": []}
        ]
      },
      "arg_rename":   {"name": "repo"},
      "field_rename": {"title": "title", "num": "number"}
    }
`
	return []llm.SystemBlock{
		{Text: catalog, CacheBreakAfter: true},
		{Text: preamble},
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
