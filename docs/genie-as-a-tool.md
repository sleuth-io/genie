# Genie — Repositioned as a Tool, Not a Service

*2026-05-06. Reframes the strategy memo (`strategy.md`) — read this one first.*

---

## The new positioning

**Genie is a single-binary smart MCP client for agent developers.**

You drop it into your agent the way you'd drop in an HTTP client library. Your agent's LLM stops loading 38 tool schemas + parsing 1700-token API responses + writing summarisation logic, and instead calls one tool that takes intent in and returns shaped data out.

**Tagline candidates** (now that the audience is devs, not CIOs):

- *Smart MCP client for agents.*
- *Stop your agent from drowning in tool calls.*
- *One tool, one binary, all your MCP servers.*
- *The MCP cache for agent developers.*

## Why this is the right reframe

The earlier framing — "managed gateway, hosted multi-tenant SaaS, sold to enterprise" — doesn't fit the org reality (Sleuth has no product surface to bolt this onto right now) and stacks every hard problem on top of every other:

- **Tenant safety** ← only matters if hosted multi-tenant.
- **Per-call pricing fights gravity** ← only matters if hosted SaaS.
- **OSS commoditization** ← only matters if competing with Portkey/LiteLLM.
- **Enterprise procurement loop** ← only matters in top-down sales.
- **6–12 month wedge before APC closes paraphrase gap** ← only matters if shipping speed gates a hosted-SaaS launch.

Reframe to **"tool you embed in your agent"** and almost all of those evaporate:

| Old concern (hosted gateway) | New status (single binary) |
|---|---|
| Cross-tenant cache leakage | N/A — cache is local to your agent. |
| Per-call pricing fights free competitors | N/A — OSS. Compete on UX + integration depth. |
| Enterprise procurement | N/A — `go install` is the install. |
| 6–12 month wedge | Still real, but now a developer-mindshare race, not a sales race. Different game. |
| Confused-buyer overlap with semantic caches | Less acute — devs reading code can see the difference. |

## What stays the same (the load-bearing parts)

The product mechanics didn't change. We measured these on the working spike:

- 88% smaller tool-result payloads (only the requested fields).
- 53% faster wall-clock end-to-end.
- 27% fewer caller-side tokens.
- 0 LLM tokens on cache replay.
- 100% accuracy on first-call resolution across 16 curated queries.
- 0% false-positive cache collisions on 14 adversarial pairs.

These are the win regardless of how it's distributed. They just sell into a different ICP now.

## ICP — agent developers, not enterprise CIOs

- **Bullseye**: developers building agentic products that already use 1+ MCP servers and are starting to feel the context bloat. Concretely: anyone configuring 3+ MCP servers in their `.mcp.json`, or anyone whose LLM-tool-use loop has visibly slowed as the tool surface grew.
- **Adjacent**: framework authors (LangChain/LlamaIndex/etc. agent runners) who want a "smart MCP client" they can recommend or bundle.
- **Validating use case**: `kit` (`/home/mrdon/dev/kit`) — an existing Go agent platform that needs to make tool calls, increasingly via MCP, and wants to do it cleanly. **kit is the design partner for free**.

## Three modes Genie ships in

The existing spike already supports two of these. The third is a small refactor.

### Mode 1 — MCP server / sidecar (works today)

```bash
go install github.com/mrdon/genie@latest
```

Add to your `.mcp.json`:

```json
{
  "mcpServers": {
    "genie": {
      "command": "genie",
      "args": ["serve"],
      "env": { "GENIE_ENV_FILE": "/path/to/.env" }
    }
  }
}
```

Your agent's LLM sees one tool: `run_query(provider, query)`. The Genie subprocess handles MCP-client logic, LLM-driven plan generation, caching, and response shaping. Cache lives in `./crystallized/` next to your project.

This is what the spike already does. Zero-effort for the integrating agent.

### Mode 2 — Go library (small refactor)

```go
import "github.com/mrdon/genie"

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

For Go agent frameworks (kit, custom Go agents). Tighter integration; no subprocess overhead.

This needs the existing internal packages (`engine`, `crystallize`, `plan`, `mcpclient`) to be reorganized under a clean `pkg/genie/` import path. ~half a day's refactor.

### Mode 3 — CLI (works today)

```bash
genie query --provider github '{ viewer { login } }'
genie eval --cold --replay
```

Already shipped as `intent-gw query` / `intent-gw eval`. Used for testing, debugging, and scripting.

## How `kit` would adopt Genie

`kit` is currently an MCP server with monty-based scripting. It will soon need to call external MCP servers. Two integration paths:

**Path A — Genie as `kit`'s sidecar.** kit's `.mcp.json` (or equivalent) spawns Genie. kit's LLM sees `run_query` for any external API. **Zero code changes in kit.** This is the demo-friendliest way to show value: turn it on, watch the token bill drop on kit's existing eval set.

**Path B — Genie as a Go library inside kit.** kit imports `pkg/genie` and routes its external-MCP tool calls through it. Tighter, no subprocess — but requires Mode 2 to land first.

**Recommendation**: ship Path A this week. kit's existing config picks it up; we measure kit's actual workload against the 27/88/53 numbers and find out whether the spike's GitHub-eval numbers hold on kit's real traffic. If they do, Path B becomes the productionisation step.

## Distribution & monetization (open questions, no commitments yet)

The "tool, not service" framing opens several possibilities. Pick later, after kit validates:

- **Pure OSS, no monetization.** Just ship it. Sleuth gets brand credit; Genie gets developer mindshare; Sleuth's eventual hosted product (when there is one) has a built-in adoption funnel.
- **Open core.** Free single-binary; paid hosted version with cross-tenant cache, audit logs, governance. Competes head-on with Portkey-tier pricing if/when Sleuth wants a hosted product.
- **Bundled into kit (or kit-equivalent product line).** If kit becomes a paid product, Genie ships inside it as a feature. The OSS standalone is the funnel.
- **Acquisition path.** Build it well, get it adopted in 1000+ agent projects, sell to Anthropic / Cloudflare / a tools-AI player. Portkey just exited to Palo Alto Networks for Prisma AIRS — there's an active M&A market.

These aren't mutually exclusive. The OSS-first ship doesn't foreclose any of them.

## What changes in the next 7 days

1. **Refactor `cmd/intent-gw` → `cmd/genie`.** Rename binary, package, README. The existing code mostly Just Works; it's positioning, not engineering.
2. **Pin Mode 1 instructions in the README.** "How to add Genie to your agent in 30 seconds" should be the top of the README.
3. **Wire Genie into kit as a sidecar.** Take kit's existing `.mcp.json` (or whatever kit uses for MCP server config), add a `genie` entry pointing at our binary, point kit at a real external MCP server (GitHub), measure.
4. **Drop the strategy-memo framing publicly.** The `strategy.md` "Sleuth-feature path + design-partner overlay" pitch was right for a hosted-service business; it's wrong for a tool. Keep `strategy.md` for record but mark it superseded by this doc.
5. **Pick a tagline.** *Smart MCP client for agents* is the working candidate.

## What this reframe doesn't fix

- **APC prior art is still real.** Plan caching is still a published technique. The combination (plan + paraphrase + response shaping + single-binary distribution) is still defensible — but the algorithm itself isn't novel and shouldn't be sold as if it is.
- **Anthropic could still ship paraphrase-tolerant intent caching.** If they do, even a OSS tool gets squeezed. Monitor.
- **OSS economics are real.** "Just ship the binary" doesn't pay anyone's salary. Eventually the OSS-vs-hosted-vs-bundled question has to get answered. But it's a Q4-or-later question, not a today question.

## Decision points (now smaller and crisper)

- **Confirm the reframe.** Are we shipping Genie as an OSS tool with kit as the design partner, or sticking with the hosted-gateway plan?
- **Rename.** "Genie" stands; binary becomes `genie` (was `intent-gw`).
- **Kit integration this week.** Sidecar mode, real measurements, real workload.
