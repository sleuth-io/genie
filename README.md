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


## Quickstart

```bash
go install github.com/sleuth-io/genie/cmd/genie@latest
```

Add the MCP servers you want Genie to front:

```bash
# HTTP/SSE with OAuth — opens a browser, finishes the well-known dance,
# stores tokens in your OS keychain. One command, then you're done.
genie mcp add -n linear -u https://mcp.linear.app/sse

# stdio
genie mcp add -n github -c "github-mcp-server stdio" \
  --env 'GITHUB_PERSONAL_ACCESS_TOKEN=${env:GITHUB_TOKEN}'
```

Point your agent (Claude Code, Claude Desktop, etc.) at Genie:

```json
{
  "mcpServers": {
    "genie": { "command": "genie", "args": ["serve"] }
  }
}
```

Your agent's LLM now sees `run_query(provider, query)` and `list_providers()`. It writes GraphQL-shaped intent — invented field names are fine — and Genie maps it to whatever the upstream MCP server actually returns:

```graphql
{ pull_requests(owner: "anthropics", repo: "anthropic-sdk-python", state: "open") { title number author { login } } }
```
```graphql
{ openPRs(owner: "anthropics", repo: "anthropic-sdk-python") { title num author { username } } }
```

Both phrasings resolve to the same cached plan. The first call pays the LLM-generation cost; every subsequent call — including paraphrases — hits the cache and pays nothing.

## How it works

1. **Schemaless GraphQL parse.** The query becomes a node tree. Field names are treated as intent, not validated against any schema.
2. **Two-level cache** under `~/.cache/genie/crystallized/<provider>/`:
   - **L1** keyed by literal-shape hash → points at an L2 entry plus per-query rename info.
   - **L2** keyed by **canonical-shape hash** (LLM-normalised) holding the resolution script and canonical I/O contract.
3. **L1 hit** → load script, apply cached rename, run. Zero LLM tokens.
4. **L1 miss → small NORMALIZE call** produces a canonical hash + rename. **L2 hit** → reuse the script, write an L1 alias for next time. **L2 miss → full GENERATE call** emits a Python (Monty/WASM) script that resolves this intent against the upstream MCP server's tools. Cache it.
5. **Engine** runs the script in a sandbox, applies the per-query rename to the canonical-keyed output, recurses into child nodes (each may trigger its own L1/L2/GEN cycle).

The "rename" step is what lets `commits { hash }` work when GitHub returns `sha` — the GENERATE call writes the script to canonical names, and the per-query alias remaps the output for the caller.

## LLM backend

Plan generation needs an LLM. Genie picks one automatically:

1. `ANTHROPIC_API_KEY` set → Anthropic API directly.
2. Otherwise, `claude` CLI on PATH → shell out to it. Useful when Genie runs under Claude Code; no API key required.
3. Otherwise → error.

Pin one with `GENIE_LLM_BACKEND=anthropic-sdk|claude-cli`.

Genie makes two distinct LLM calls per cache-miss node:

- **NORMALIZE** — small structured-JSON output (canonical schema + rename maps). Sonnet/Haiku is plenty.
- **GENERATE** — writes a Python script that invokes upstream MCP tools. Benefits from Opus's reasoning.

Override per-call-type with `GENIE_NORMALIZE_MODEL` and `GENIE_GENERATE_MODEL`. Canonical IDs (verified against the Anthropic SDK; see `internal/llm/models.go` for the single source of truth):

| Model | ID |
|---|---|
| Opus 4.7 | `claude-opus-4-7` |
| Sonnet 4.6 | `claude-sonnet-4-6` |
| Haiku 4.5 | `claude-haiku-4-5` |

The claude CLI also accepts shorthand aliases (`opus`, `sonnet`, `haiku`). Empty env var ⇒ backend default (`claude-opus-4-7` for the SDK; Claude Code's session default for the CLI).

## Authentication

stdio servers authenticate with env vars (`--env`). HTTP/SSE servers use OAuth 2.1 + PKCE with RFC 8414/9728 well-known discovery and RFC 7591 dynamic client registration — no per-provider client setup needed. `genie mcp add` opens the browser flow inline; tokens land in your OS keychain (Keychain / Secret Service / Credential Manager). Refresh is automatic; on revocation the next request re-runs the flow.

`genie auth <provider>` re-runs the flow manually. `genie auth list` shows status. `GENIE_AUTH_BACKEND=file` falls back to disk for headless boxes without a keyring.

## Status

Working spike with measured wins on a curated GitHub eval set. **Not production-grade** — single-tenant, no governance layer. Multi-provider routing works; failed providers are logged and dropped, not fatal. Lazy spawn, health checks, and OAuth-flow handoffs are deferred.

## Why this and not Portkey / LiteLLM / GPTCache

Existing LLM gateways cache *responses* by semantic similarity — the data goes stale, and false-positive cache collisions are a known failure mode (0.8–99% in published research). Genie caches the *resolution plan* — the plan replays against fresh data on every call. Different category, different failure modes.

Existing MCP gateways (Cloudflare AI Gateway, etc.) route and observe MCP traffic but don't shape responses or cache by intent. Genie sits one layer up — between your agent's LLM and the MCP servers it would otherwise hammer directly.
