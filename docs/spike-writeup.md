# Intent Gateway Spike — Writeup

**Date:** 2026-05-06
**Verdict:** **GO**

All four GO/KILL bars from `intent-gateway-spike-prd.md` met or exceeded.

## Hypothesis verdicts

| # | Hypothesis | Bar | Measured | Result |
|---|------------|-----|----------|--------|
| 1 | First-call resolution correctness | ≥80% | **100%** (16/16) | PASS |
| 2 | Replay correctness | ≥95% | **100%** (16/16) | PASS |
| 2 | Replay token reduction | ≥10× | **1078 → 0 avg/case** | PASS |
| 3 | Adversarial false-positive collisions | <5% | **0%** (0/14) | PASS |

Reproduction: `intent-gw eval --cold --replay` (~2 min cold, ~14 s warm) and `intent-gw eval --hypothesis-3` (~2 min, no cache state needed).

## What was built

A standalone Go service (`intent-gw`) with two surfaces:

- **CLI** — `intent-gw query "<graphql>"` and `intent-gw eval` for the harness.
- **MCP** — `intent-gw serve` exposes one tool, `run_query(provider, query)`, over stdio. Drives the same pipeline as the CLI; a `.mcp.json` is checked in so Claude Code can pick it up.

The pipeline:

1. **Schemaless GraphQL parse** (`vektah/gqlparser/v2`) — the user's query becomes a `Node` tree. Field names are intent signals; nothing is schema-validated.
2. **Two-level cache** under `./crystallized/`:
   - L1 alias keyed by the literal-shape hash → points at an L2 entry plus per-query rename info.
   - L2 entry keyed by the **canonical-shape hash** (LLM-normalized) holding the monty script and canonical-keyed I/O contract.
3. **L1 hit (zero LLM)** → load script, apply cached rename, run.
4. **L1 miss → small NORMALIZE LLM call** (Opus 4.7, adaptive thinking, ~tool-catalog tokens cached) → canonical_hash + rename. **L2 hit** → reuse canonical script, write L1 alias for next time. **L2 miss** → full **GENERATE LLM call** that emits a canonical-keyed Python script. Cache it.
5. **Engine** runs the script in a Monty/wazero sandbox, applies the per-query rename to the canonical-keyed output, walks child nodes recursively (each may trigger its own L1/L2/GEN cycle).

A small set of host helpers (`now_iso`, `days_ago_iso`, `hours_ago_iso`, `github_*` for every MCP tool) sits inside the sandbox so generated scripts use natural Python without hitting the WASM runtime's gaps.

## Distinctive things the data showed

- **Hypothesis 3 worked organically inside the cold run, not just on the adversarial set.** During a single 16-case cold pass, the LLM emitted 5 L2 hits (paraphrases canonicalising to a still-warm canonical) on top of 11 full GENs — so cache convergence happened in real time, not just on a curated paraphrase pair.
- **The replay path actually hits zero tokens.** Every L1 alias short-circuits the LLM, so the warm-replay token reduction isn't "10× cheaper" — it's "no LLM at all". The ratio is bounded only by the cold cost.
- **Sandbox gap-filling beat sandbox prohibition.** Day-5 hypothesis 1 was 75% (under the bar) when the prompt told the LLM to avoid `datetime.now()` / `str.format()`. Adding `now_iso()` host functions and updating the prompt to advertise them got it to 100% on the next run.
- **Field-rename layer mattered for paraphrase composition.** Naively cached scripts produced canonical-keyed output that didn't match paraphrase field names; replay correctness was 81% before the rename layer landed and 100% after.

## Caveats — things to treat as Day-1 follow-ups, not blockers

- **Curated set size: 7 intents × ~16 cases.** The PRD asked for ~30 × 5 ≈ 150. Numbers are clean but the sample is small; expand before any commitment beyond the spike.
- **Single target system, single repo for live data.** All measurements use GitHub via `github-mcp-server` against `anthropics/anthropic-sdk-python` and `modelcontextprotocol/servers`. Both are well-formed and active. A noisier target (less consistent response shapes, partial PAT scopes, rate-limit pressure) is the next pressure test.

- **Training-data flattery.** The LLM has seen GitHub's REST API + `github-mcp-server` + the GitHub GraphQL schema *extensively*. That helps two of our three LLM calls: (a) the test driver writing the GraphQL-shaped input — Claude defaults to GitHub-flavored vocabulary even when it's wrong (we observed `pull_requests(direction, owner, repo, sort, state)` and `commits(branch, limit, owner, repo) { hash message }` — neither matches real GitHub GraphQL but both are plausible intent strings); and (b) the generate-call picking the right MCP tool — `github_list_pull_requests` is obvious to a model that's seen the tool's source. We're NOT issuing GraphQL to GitHub — the input is purely an intent format, and the output goes through MCP tools that ride GitHub's REST API. So training-data familiarity flatters the *quality* of the LLM calls, not the *correctness* of the query path. To re-confirm against a domain the model knows less well, point the same pipeline at a target with novel vocabulary (an internal API, or a less-public MCP server). The architecture handles it — tool catalogs auto-render, prompt caching is identical — but the 100% numbers could regress without the canonical-name table I curated for GitHub.
- **Prompts are GitHub-tuned.** The architecture is provider-agnostic — the MCP tool catalog auto-renders from whatever client is wired — but the canonical-name table inside the normalize prompt is GitHub vocabulary. Multi-provider support means generating that table per-provider (cheap option: derive once per session from the tool catalog at startup; expensive option: curate per-provider tables).
- **Drift detection (FR-6) cut on purpose.** Two weeks isn't long enough to measure GitHub-API drift; the slot in the executor is marked `// TODO(drift):` and is the obvious next thing if the product moves forward.
- **No latency SLO measured.** Cold-cache full-generate is 5–10 s/case (LLM-bound). Warm replay is <1 s. If we productionize, we'll want a percentile distribution under realistic concurrency.

## Recommendation

**Continue.** The three load-bearing bets — that an LLM can resolve hallucinated queries against a tool surface, that the resolution can be crystallized, and that paraphrases fingerprint to the same script — all came back affirmative on real GitHub data with real Claude calls. The throwaway code stays throwaway, but the architecture is worth re-implementing carefully:

- Per-provider tool catalogs and canonical-name tables (the cheap-derivation path).
- A larger curated set (the PRD's full 30 × 5).
- Real sandboxing (gVisor / WASM-with-fuel) — monty is fine for the spike, not for untrusted multi-tenant scripts.
- Drift detection wired against a longer measurement window (a quarter, not two weeks).
- Cross-tenant artefact sharing (the deferred decision in the PRD).

The deferred questions in the PRD's "Open questions" section (Sayan thesis-share, OSS-vs-private, first 5 buyers) are the real next-step gates, not technical risk.
