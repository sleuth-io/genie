# Genie — Consolidated Strategy Memo

*Drafted 2026-05-06 from two subagent research passes (competitive disproof + GTM survey) and the spike's own measured numbers. Treat as a working document, not a final commitment.*

---

## The position that survives scrutiny

Genie is a managed gateway between AI agents and external APIs. Its load-bearing claim sits at a **three-way intersection** that no single competitor currently occupies:

1. **Response shaping** — the cached plan returns only the requested fields (88% measured payload reduction vs. raw MCP tool result).
2. **Paraphrase amortization** — same intent in different words → one cached resolution plan, replayed.
3. **Cross-tenant plan reuse** — first customer's first call subsidizes every subsequent customer's matching call.

Stated externally:

> *Tool Search trims the menu. Genie remembers the recipe and trims the plate.*

Genie sells **with** Anthropic's shipped features (Tool Search, PTC, prompt caching), not against them. Customers using all three see the wins compound.

## What changed since the spike PRD

The original PRD framed the win partly as **catalog bloat reduction** — fewer tool schemas in agent context. Anthropic shipped Tool Search + Programmatic Tool Calling to GA on Feb 17 2026, which fixes the catalog-bloat half of that pitch for free. **Drop the catalog-bloat framing from all marketing copy**; it's no longer ours to sell.

What Anthropic did NOT solve:
- Tool-result payload size (the API returns whatever it returns; Genie's 88% reduction is on this surface).
- Paraphrase amortization (different wordings of the same intent still each pay full price under Anthropic's stack).
- Cross-call / cross-tenant cache (every invocation, every customer pays from scratch).

Those three are the surviving wedge.

## Re-ranked threats (highest first)

| # | Threat | Severity | Mitigation |
|---|---|---|---|
| 1 | **Agentic Plan Caching (arXiv 2506.14852)** is published, NeurIPS 2025 accepted, with Jan 2026 follow-on work. The technique is no longer novel; anyone can read the recipe. | High | Combination is what's defensible. APC explicitly rejects fuzzy matching (paraphrase tolerance) and doesn't address response shaping. Ship the combination fast. |
| 2 | **Confused-buyer overlap with semantic caches** (Portkey, LiteLLM, GPTCache, Helicone). They ship *response* caching with stale-data risk; procurement may not see the difference. | Medium-high | Education + positioning. "We cache plans, not answers — the data stays fresh." Combine with the cross-tenant amortization story. |
| 3 | **Cross-tenant cache leakage** if paraphrase tolerance is implemented via embeddings. Semantic caches show 0.8%–99% false-positive rates in published research. | Medium | Use canonicalised plan keys (LLM-normalised, deterministic), not embedding similarity. APC's exact-keyword approach defends; Genie's L2 hash is a closer cousin to that than to embeddings. |
| 4 | **Paraphrase wedge closes** within 6–12 months as APC follow-on work adds fuzzy matching. | Medium | Time-bound — execution speed matters more than algorithmic novelty. Get to customer-cache-density before the paper closes. |
| 5 | **Anthropic ships intent caching themselves.** No public hint today, but they ship fast. | Low (today) | Monitor changelog. Build the cross-tenant amortization moat (which Anthropic structurally cannot own — they don't have customer-customer cache visibility). |

## Strategic implications

1. **The moat is customer cache density, not algorithmic novelty.** First to N customers with M unique query shapes wins. The cross-tenant hit rate compounds as the catalog of shapes fills.

2. **Speed of distribution > technical sophistication.** A 6–12 month lead on paraphrase tolerance is real, but only if we use it. Building the third version of an MCP gateway in public takes 6 months by itself.

3. **Sleuth's existing distribution is the unfair advantage.** Sleuth has a 7-year relationship with engineering leadership at companies shipping software — exact buyer overlap with "AI platform team / agent product founder" ICPs. That distribution is worth more than the algorithm.

4. **Don't try to win the OSS-bottoms-up race.** Portkey, LiteLLM, Helicone, Cloudflare AI Gateway, Obot already hold that lane. Per-call pricing has commoditized to free.

## Recommended GTM (consolidated from the GTM subagent)

**Path: Sleuth-feature + design-partner overlay.**

- Bundle Genie into Sleuth's existing AI-governance product line as the *performance & cost* pillar alongside the *governance* pillar. Don't spin out as a standalone product.
- 3–5 lighthouse design partners from Sleuth's existing customer base. Co-build, charge premium, publish the 27/88/53 numbers as proof.
- Pricing: bundled in Sleuth enterprise contracts. **No per-call fee** — that fights industry gravity.
- OSS only the client SDK, not the gateway. The OSS lane is closed.
- Position publicly as complementary to Anthropic's stack, not competitive.

What this is NOT:
- Not pure OSS bottoms-up (closed lane).
- Not standalone enterprise top-down (wastes Sleuth's existing leverage).
- Not acquisition-bait (premature without revenue, but option D remains plausible — Portkey just sold to Palo Alto Networks for Prisma AIRS, so there's an active M&A market).

## What to ship first

| Quarter | Focus | Why |
|---|---|---|
| Q3 2026 | Design-partner-grade gateway: 3–5 customers, multi-provider beyond GitHub (Linear, Notion), per-tenant cache binding, basic audit log | Customer numbers > technical features. Need real production data. |
| Q4 2026 | Cross-tenant cache safety review + governance layer (RBAC, plan approval, redaction) | These are table-stakes for Sleuth's enterprise motion and the differentiator vs. OSS gateways. |
| Q1 2027 | GA inside Sleuth Skills / equivalent product line | By this point either we've built the customer cache density, or we haven't and the wedge has closed. |

## Open questions / what we still don't know

- Whether Genie's 27/88/53 numbers hold beyond GitHub. Training-data flattery is real; Linear/Notion need to be the next-pressure-test domains.
- Sleuth's actual ARR / Skills paying-customer count — needed to size the bundling motion realistically.
- Whether MCP's June 2026 spec (TTL/ETag fields) standardises anything that would commoditise plan keys. Track.
- Whether Anthropic's roadmap includes cross-call tool memoisation. If it ships within 12 months, the wedge closes entirely and this becomes an acquisition play.

## Decision points for the founders

- **Commit to the Sleuth-feature path** — yes/no/hybrid? This affects everything downstream (hiring, pricing, branding, fundraising).
- **Open-source the gateway, or hold it closed?** GTM agent says hold; one alternative argument is that OSS'ing it now is the only way to compete with Portkey-style distribution. Worth a real debate.
- **Name & brand** — "Genie" reads as a feature inside Sleuth; "Sleuth Genie" is fine. A standalone brand would imply a spin-out, which contradicts the recommended GTM.
