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


## 30 seconds: add Genie to your agent

```bash
go install github.com/sleuth-io/genie/cmd/genie@latest
```

Add the MCP servers you want Genie to front. No config file to edit — `genie mcp add` writes it for you:

```bash
# A stdio MCP server (env-var auth)
genie mcp add github github-mcp-server stdio \
  --env 'GITHUB_PERSONAL_ACCESS_TOKEN=${env:GITHUB_TOKEN}' \
  --description 'GitHub repos, PRs, issues'

# An HTTP/SSE MCP server with OAuth — Genie pops a browser, finishes the
# RFC 8414 + RFC 7591 dance, stores tokens in your OS keychain.
genie mcp add linear https://mcp.linear.app/sse --type sse --scope read --scope write

# Anything you can paste from Claude Code's .mcp.json works verbatim:
genie mcp add --json '{"name":"atlassian","url":"https://mcp.atlassian.com/v1"}'

# Inspect what's wired up:
genie mcp list
genie auth list
```

Tokens land in your OS keychain (Keychain on macOS, Secret Service on Linux, Credential Manager on Windows); refresh is automatic. To re-authorize manually, run `genie auth <provider>`. The config itself lives at `~/.config/genie/config.json` if you ever want to edit it directly — it's the same JSON shape Claude Code uses, so you can paste yours in.

Then point your agent at Genie:

```json
{
  "mcpServers": {
    "genie": {
      "command": "genie",
      "args": ["serve"],
      "env": {
        "ANTHROPIC_API_KEY": "${env:ANTHROPIC_API_KEY}"
      }
    }
  }
}
```

Your agent's LLM now sees two tools: `run_query(provider, query)` for resolution and `list_providers()` for introspection. The `provider` arg is one of the keys you put in `mcpServers` (above: `github`, `linear`).

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

- **Front many MCP servers, expose two tools.** Configure as many upstream MCP servers as you want; your agent's LLM still only sees `run_query` and `list_providers`. No more "load 38 schemas to call one of them."
- **Pre-shaped responses.** Genie returns only the fields your agent asked for. Raw API responses (often 1.5K+ tokens of metadata) never enter your agent's context.
- **Plan caching, not response caching.** The *resolution plan* is cached by canonical intent — the data stays fresh, the planning cost amortises. Paraphrases of the same question hit the same cached plan.
- **Hallucination-tolerant input.** Your agent can ask in any GraphQL-shaped phrasing, including invented field names — Genie maps them to the real underlying fields. *Make a wish, get the data.*

## Three modes

### Mode 1 — sidecar / MCP server (recommended; works today)

What the quickstart above describes. Your agent spawns `genie serve` as a subprocess, sees `run_query` + `list_providers`. Cache lives at `~/.cache/genie/crystallized/` (or `$GENIE_CACHE_DIR`), shared across projects so paraphrased queries from any working directory hit the same plans.

### Mode 2 — Go library (in-process)

```go
import "github.com/sleuth-io/genie/pkg/genie"

g, err := genie.New(ctx, genie.Config{
    Providers:    []genie.Provider{genie.GitHubMCP(token)},
    AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
})
defer g.Close()

result, err := g.Query(ctx, genie.QueryRequest{
    Provider: "github",
    Query:    `{ pull_requests(state: "open") { title number author { login } } }`,
})
```

For Go agent frameworks that want tighter integration without a subprocess. Pass `Providers` programmatically (as above) or set `ConfigPath` to point at a JSON file in the same shape as the quickstart config.

### Mode 3 — CLI (debugging, scripting, eval)

```bash
genie query --provider github '{ viewer { login } }'
genie eval --cold --replay     # run the bundled eval set
```

## Subcommands

Provider management:

- `genie mcp add <name> <url|command> [args...]` — register an MCP server. URL → http/sse transport (auto-runs OAuth flow); command → stdio.
- `genie mcp add --json '{...}'` — same, fed an entry from Claude Code's `.mcp.json` verbatim.
- `genie mcp list` — show configured providers.
- `genie mcp remove <name>` — drop a provider (also clears its stored credentials unless `--keep-credentials`).

Auth:

- `genie auth <provider>` — re-run the OAuth browser flow for an http/sse provider.
- `genie auth list` — show which providers are authenticated and when tokens expire.
- `genie auth logout <provider>` — drop stored credentials for one provider.

Runtime:

- `genie serve` — start MCP stdio server exposing `run_query` + `list_providers`.
- `genie query [--provider NAME] "<graphql>"` — resolve one query, print JSON.
- `genie eval [--cold] [--replay] [--hypothesis-3]` — run the curated and adversarial sets, print metrics.

## Authentication

Genie supports stdio MCP servers (env-var auth, e.g. `GITHUB_PERSONAL_ACCESS_TOKEN`) and HTTP/SSE servers with OAuth 2.1 + PKCE. The OAuth flow uses RFC 8414 + RFC 9728 well-known discovery and RFC 7591 dynamic client registration, so no per-provider client setup is needed for compliant servers.

Tokens are stored in the OS keychain by default (override with `GENIE_AUTH_BACKEND=file` for headless boxes without a keyring). Refresh tokens are used automatically when access tokens expire; on refresh failure or token revocation, the next request re-runs the browser flow.

## Configuration

- Config: `$GENIE_CONFIG` or `~/.config/genie/config.json` (per `os.UserConfigDir`).
- Cache: `$GENIE_CACHE_DIR` or `~/.cache/genie/crystallized/` (per `os.UserCacheDir`). Each provider gets its own subdirectory.
- Env vars in config use `${env:VAR}` interpolation. Unset variables are a hard error so a typo can't silently spawn an MCP server with an empty token.

## LLM backend

Plan generation needs an LLM. Genie picks a backend automatically:

1. `ANTHROPIC_API_KEY` set → call the Anthropic API directly.
2. Otherwise, if the `claude` CLI is on `PATH` → shell out to it. Useful when Genie is running under Claude Code; no API key required.
3. Otherwise → error.

Pin a specific backend with `GENIE_LLM_BACKEND=anthropic-sdk` or `GENIE_LLM_BACKEND=claude-cli`.

## Layout

- `cmd/genie/` — entry points (`query`, `serve`, `eval`)
- `pkg/genie/` — public Go API (`New`, `Config`, `Query`, `ListProviders`, …)
- `internal/config/` — config-file parser (`mcpServers` schema + env interpolation)
- `internal/auth/` — OAuth flow + keychain/file-backed token vault
- `internal/providers/` — multi-provider lifecycle (spawn, route, close)
- `internal/runtime/` — embedded monty (Python-on-WASM via wazero); script execution sandbox
- `internal/engine/` — schemaless GraphQL parser, node-shape hashing, executor
- `internal/plan/` — Anthropic SDK calls for plan generation + canonical-shape normalisation
- `internal/crystallize/` — flat-JSON cache (L1 alias + L2 entry, namespaced per provider)
- `internal/mcpclient/` — MCP stdio client + bridge from script-side host functions to MCP tool calls
- `internal/mcpserver/` — exposes `run_query` + `list_providers` to upstream agents
- `internal/eval/` — eval harness + ground-truth fixtures + FR-7 metrics
- `eval/intents.yaml`, `eval/adversarial.yaml` — curated test sets

## Status

Working spike with measured wins on a curated GitHub eval set. **Not production-grade** — single-tenant, no governance layer. Multi-provider routing works today; failed provider spawns are logged and dropped, not fatal. Lazy provider spawn, health checks, and OAuth-flow handoffs are deferred.

## Why this and not Portkey / LiteLLM / GPTCache

Existing LLM gateways cache *responses* by semantic similarity — the data goes stale, and false-positive cache collisions are a known failure mode (0.8–99% in published research). Genie caches the *resolution plan* — the plan replays against fresh data on every call. Different category, different failure modes.

Existing MCP gateways (Cloudflare AI Gateway, etc.) route and observe MCP traffic but don't shape responses or cache by intent. Genie sits one layer up — between your agent's LLM and the MCP servers it would otherwise hammer directly.
