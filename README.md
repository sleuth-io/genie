# Genie

**Smart MCP client for agents.** A single binary you drop into your agent. Your LLM stops loading 38 tool schemas, parsing 1700-token API responses, and writing summarisation logic. Instead, it writes a short **GraphQL-shaped query** describing what it wants, hands it to Genie's one tool (`run_query`), and gets back exactly the fields it asked for. Genie caches the resolution plan by canonical intent — different phrasings of the same question hit the same cache and pay nothing.

Field names in the query don't have to match any real schema — they're treated as **intent signals**, not schema references. Your LLM can ask for `commits { hash, message }` even though GitHub's API returns `sha`. Genie maps it.

> Tool Search trims the menu. Genie remembers the recipe and trims the plate.

Measured against the GitHub MCP server on a representative query:

| | Without Genie | With Genie | Δ |
|---|---|---|---|
| Tool-result payload | 6,694 chars | 803 chars | **−88%** |
| Caller-side tokens | 92,758 | 68,047 | −27% |
| Wall time | 18.7 s | 8.9 s | **−53%** |
| LLM tokens on cache replay | full | **0** | — |

See `docs/spike-writeup.md` for the full eval (16 curated queries, 14 adversarial pairs) and `docs/genie-as-a-tool.md` for the product framing.

## 30 seconds: add Genie to your agent

```bash
go install github.com/mrdon/genie/cmd/genie@latest
```

In your agent's `.mcp.json` (or equivalent MCP-server config):

```json
{
  "mcpServers": {
    "genie": {
      "command": "genie",
      "args": ["serve"],
      "env": {
        "GENIE_ENV_FILE": "/abs/path/to/your/.env"
      }
    }
  }
}
```

Your agent's LLM now sees one new tool: `run_query(provider, query)`. Point it at a target API (today: `github`; multi-provider next) by adding the credentials to that `.env`:

```bash
GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx
ANTHROPIC_API_KEY=sk-ant-xxx
go install github.com/github/github-mcp-server/cmd/github-mcp-server@latest
```

**What your agent's LLM does:** writes a GraphQL-shaped query string and passes it to `run_query`. The field names don't have to match any real schema — they're treated as intent signals. So either of these is valid input:

```graphql
{ pull_requests(owner: "anthropics", repo: "anthropic-sdk-python", state: "open") { title number author { login } } }
```
```graphql
{ openPRs(owner: "anthropics", repo: "anthropic-sdk-python") { title num author { username } } }
```

Both resolve to the same cached plan. The first call for a new query shape pays an LLM-generation cost; every subsequent call — including paraphrases like the second example — hits the cache and pays nothing.

(If your agent takes natural-language input from a human, the human→GraphQL translation happens in the *caller's* LLM, not in Genie. That's a feature: agents are good at writing GraphQL-shaped intent; they're bad at parsing 1700-token API responses. We do the second half.)

## What it does

- **One tool, not dozens.** Your agent's LLM sees `run_query`, not the underlying MCP server's full tool catalog. No more "load 38 schemas to call one of them."
- **Pre-shaped responses.** Genie returns only the fields your agent asked for. Raw API responses (often 1.5K+ tokens of metadata) never enter your agent's context.
- **Plan caching, not response caching.** The *resolution plan* is cached by canonical intent — the data stays fresh, the planning cost amortises. Paraphrases of the same question hit the same cached plan.
- **Hallucination-tolerant input.** Your agent can ask in any GraphQL-shaped phrasing, including invented field names — Genie maps them to the real underlying fields. *Make a wish, get the data.*

## Three modes

### Mode 1 — sidecar / MCP server (recommended; works today)

What the quickstart above describes. Your agent spawns `genie serve` as a subprocess, sees one MCP tool. Local cache lives in `./crystallized/`.

### Mode 2 — Go library (in-process)

```go
import "github.com/mrdon/genie/pkg/genie"

g, err := genie.New(genie.Config{
    Providers:    []genie.Provider{genie.GitHubMCP(token)},
    AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
    CacheDir:     "./crystallized",
})

result, err := g.Query(ctx, genie.QueryRequest{
    Provider: "github",
    Query:    `{ pull_requests(state: "open") { title number author { login } } }`,
})
```

For Go agent frameworks that want tighter integration without a subprocess. Coming next.

### Mode 3 — CLI (debugging, scripting, eval)

```bash
genie query --provider github '{ viewer { login } }'
genie eval --cold --replay     # run the bundled eval set
```

## Subcommands

- `genie query "<graphql>"` — resolve one query, print JSON.
- `genie serve` — start MCP stdio server exposing `run_query(provider, query)`.
- `genie eval [--cold] [--replay] [--hypothesis-3]` — run the curated and adversarial sets, print metrics.

## Layout

- `cmd/genie/` — entry points (`query`, `serve`, `eval`, …)
- `internal/runtime/` — embedded monty (Python-on-WASM via wazero); script execution sandbox
- `internal/engine/` — schemaless GraphQL parser, node-shape hashing, executor
- `internal/plan/` — Anthropic SDK calls for plan generation + canonical-shape normalisation
- `internal/crystallize/` — flat-JSON cache under `./crystallized/` (L1 alias + L2 entry)
- `internal/mcpclient/` — MCP stdio client + bridge from script-side host functions to MCP tool calls
- `internal/mcpserver/` — exposes the `run_query` tool to upstream agents
- `internal/eval/` — eval harness + ground-truth fixtures + FR-7 metrics
- `eval/intents.yaml`, `eval/adversarial.yaml` — curated test sets

## Status

Working spike with measured wins on a curated GitHub eval set. **Not production-grade** — single-tenant, no governance layer, GitHub-only out of the box. Multi-provider (Linear, Notion, generic MCP) and the Go-library import path (`pkg/genie`) are next.

## Why this and not Portkey / LiteLLM / GPTCache

Existing LLM gateways cache *responses* by semantic similarity — the data goes stale, and false-positive cache collisions are a known failure mode (0.8–99% in published research). Genie caches the *resolution plan* — the plan replays against fresh data on every call. Different category, different failure modes.

Existing MCP gateways (Cloudflare AI Gateway, etc.) route and observe MCP traffic but don't shape responses or cache by intent. Genie sits one layer up — between your agent's LLM and the MCP servers it would otherwise hammer directly.

## Origin

Started as a 2-week spike validating three hypotheses (resolution feasibility, replay correctness, paraphrase fingerprinting). All three passed on real data. See `docs/intent-gateway-spike-prd.md` for the original PRD, `docs/spike-writeup.md` for the GO/KILL writeup, and `docs/genie-as-a-tool.md` for the post-spike product framing.
