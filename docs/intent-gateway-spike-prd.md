# Intent Gateway — Technical Spike PRD

**Status:** Draft
**Owner:** Don
**Timebox:** 2 weeks
**Date:** 2026-05-06

---

## Purpose

Validate or kill the riskiest technical assumptions behind the Intent Gateway thesis before any product, GTM, or hiring commitment. The spike is throwaway code; the deliverable is a confidence signal.

## Hypotheses under test

1. **Resolution feasibility.** An LLM, given a hallucinated GraphQL-shaped query and access to one target system's tools/APIs, can produce a correct, field-selected response on first call **≥80% of the time** across a curated test set.
2. **Crystallization replay.** Successful first-call resolutions can be captured as deterministic scripts that, on a second call with the same intent (verbatim or paraphrased), return correct data **≥95% of the time** with **≥10× token reduction**.
3. **Intent fingerprinting.** Paraphrased queries expressing the same intent can be mapped to the same crystallized script with **<5% false-positive collisions** against an adversarial test set.

If any of the three lands materially below threshold, kill or redesign before continuing.

## Scope of the spike

### In scope

- **One target system:** GitHub. Chosen because it offers a rich MCP server, REST API, and GraphQL API — letting the resolver pick across tool surfaces.
- **Single-tenant.** One hardcoded user, one PAT, one local install.
- **CLI harness.** `intent-gw query "<graphql-shaped-query>"` is enough. No UI.
- **Resolution path:**
  1. Accept GraphQL-shaped query (may contain invented fields)
  2. LLM plans a fulfillment using available GitHub tool surfaces
  3. Executes, shapes response to match requested fields
  4. Returns JSON matching the query shape
- **Crystallization:**
  - On success, persist the resolved plan as a sandboxed script (Python in a subprocess sandbox is fine for the spike — no need for full isolation)
  - Persist intent fingerprint + plan + I/O schema
- **Replay path:**
  - Fingerprint incoming query, lookup, execute crystallized script if hit
  - Fall through to fresh LLM resolution on miss
- **Drift detection (lightweight):**
  - Periodically (or on Nth call) re-run LLM resolution alongside cached replay
  - Diff outputs; flag drift, evict on threshold
- **Eval harness:**
  - Curated set of ~30 query intents, ~5 paraphrases each (~150 queries)
  - Ground truth captured by human review on first run
  - Automated comparison on subsequent runs

### Out of scope (do not build)

- Multi-tenant anything
- Cross-tenant crystallization sharing
- SSO / OAuth / DCR / CIMD
- Real sandboxing (gVisor, Firecracker, WASM) — subprocess is fine
- Audit log persistence beyond stdout
- Web UI, dashboards, or governance console
- More than one target system
- OSS packaging, install scripts, docs site
- Cost or rate-limit enforcement
- The GraphQL parser hardening (regex + best-effort is fine)

## Functional requirements

### FR-1: Query input
Accept a GraphQL-shaped string. The query MAY include fields that do not exist on any real GitHub schema. The resolver MUST treat field names as **intent signals**, not schema lookups. The resolver MUST NOT reject queries on schema-validation grounds.

### FR-2: Resolution
The resolver MUST attempt fulfillment by composing available GitHub tool surfaces (MCP server tool calls, REST, or GraphQL — resolver's choice). The resolver MUST return JSON whose top-level shape matches the requested field selection.

### FR-3: Field selection
The response MUST contain only fields the agent requested. Any additional data fetched during resolution MUST be discarded before return. This is the load-bearing context-reduction claim — measure it.

### FR-4: Crystallization
On a successful resolution (no errors raised, response shape matches request), the resolver MUST persist a replayable artifact containing:
- Intent fingerprint
- The fulfillment plan (executable script)
- The expected I/O schema

### FR-5: Replay
Subsequent queries with a matching fingerprint MUST execute the crystallized artifact without invoking the LLM. Any execution error MUST fall through to fresh resolution and evict the artifact.

### FR-6: Drift detection
On at least 1-in-N replays (configurable, suggest N=20 for the spike), run fresh LLM resolution in parallel and diff the outputs. Log mismatches; evict artifacts whose drift rate exceeds a threshold (suggest 10%).

### FR-7: Metrics emission
Every query MUST emit:
- Tokens to LLM (resolution path) or 0 (replay path)
- Wall time
- Cache hit/miss
- Correctness flag from eval harness (when ground truth exists)

## Non-functional requirements

- **Latency:** No SLO for the spike. Record actuals.
- **Reliability:** Crash on unrecoverable error. Don't fall back to "best-effort" responses — that masks the signal we're trying to measure (per project coding standards: no silent degradation).
- **Determinism on replay:** Crystallized scripts MUST NOT call any LLM during execution. If they do, crystallization failed.
- **Auth:** Single hardcoded PAT loaded from env. No rotation, no scoping, no per-user.
- **Storage:** SQLite or flat files for the artifact store. No infra.

## Test plan

### Curated query set (build by end of week 1)
~30 intents covering, e.g.:
- "Open PRs in repo X assigned to me"
- "Recent commits to branch Y by author Z"
- "PRs merged in the last 7 days with their review latency"
- "Issues labeled `bug` opened in the last month with their first-comment latency"
- Intentionally hallucinated fields (e.g., `pr.reviewerTenure`, `pr.timeToFirstReview`)

For each intent, write 5 paraphrases varying:
- Field names (`reviewerLatency` vs `timeToFirstReview`)
- Filter phrasing
- Field nesting

### Ground truth
First run captured + human-reviewed. Treat as fixture going forward.

### Adversarial fingerprint set
20 query pairs that *look similar but mean different things* (e.g., `pr.reviewerLatency` as time-to-first-review vs. time-to-merge). Used to measure fingerprint collision rate.

## Success criteria

The spike is a **GO** if all three are true:

1. ≥80% first-call correctness on curated set
2. ≥95% replay correctness AND ≥10× token reduction on second call
3. <5% false-positive fingerprint collisions on adversarial set

The spike is a **KILL** if any of the three lands materially below threshold and there is no clear engineering path to close the gap inside another 2 weeks.

The spike is a **REDESIGN** if results are split — e.g., resolution works but fingerprinting collapses. Document the redesign before continuing.

## Decisions deferred (record but do not resolve in spike)

- Sandbox technology for crystallized scripts (subprocess → eventually gVisor/WASM/Firecracker)
- Cross-tenant artifact sharing safety model (permission-stripped canonical form + per-tenant re-binding)
- Fingerprinting algorithm beyond the spike's heuristic
- Schema-introspection-assisted resolution (could materially raise FR-1 success rate)
- Intent-DSL refinement: stay as GraphQL-shaped, or move toward something looser
- Eval harness productionization

## Open questions for after the spike

- Does Sayan share enough of this thesis to commit two years to it?
- If yes, OSS-first dev distribution vs. private hosted alpha — pick before any line of product code is written
- Which 5 buyers (AI platform leads + CISOs) get the prototype demo first
