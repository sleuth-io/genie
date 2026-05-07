# Genie — Architectural Rules for Future Agents

## Core principle: Genie is a **generic** MCP gateway

Genie fronts arbitrary upstream MCP servers — GitHub, Atlassian, Linear,
Slack, Notion, anything else the user wires up via `genie mcp add`. The
runtime tool catalog (loaded from each provider on connect) is the only
source of provider-specific knowledge in the system.

## Do NOT add provider-specific code or prompt content

This rule binds the whole codebase. Concretely:

### In prompts (`internal/plan/llm.go`)

The `buildGenerateSystem` and `buildNormalizeSystem` prompts must work
verbatim against any MCP server. That means:

- **No hardcoded tool names.** Don't write `github_list_pull_requests`
  or `searchJiraIssuesUsingJql` into the preamble. Tool names live in
  the catalog block, which is rendered at runtime from the actual
  `*mcpclient.Client.Tools()` of the connected provider.
- **No hardcoded schema fields.** Don't list "issues have `body` and
  `title`, repos have `description` and `stargazers_count`" in the
  preamble. The LLM reads the actual JSON-Schema from the catalog.
- **No hardcoded canonical-name mappings.** Don't tell the LLM
  "openPRs maps to pull_requests"; the LLM derives canonical names by
  inspecting which tool in the catalog matches the user's intent.
- **Provider-neutral examples are fine — and useful.** Use abstract
  placeholder names (`thingies`, `list_things`, `filter_a`) when
  illustrating a rule. The point is to show the SHAPE of the
  transformation, not the specific names.

### In code

- **No `switch provider {}` branches.** All provider-specific behavior
  comes from `config.ProviderConfig` (which is just a JSON entry the
  user wrote) or from the live `*mcpclient.Client` (whose Tools() and
  Call() surface are generic).
- **No special-case packages per provider.** `internal/mcpclient/`
  has a thin `OpenGitHub()` wrapper that's a convenience constructor;
  the underlying `Open(ProviderSpec)` is generic and that's the path
  every other provider takes. Don't add `linear.go` / `atlassian.go`
  with special handling.
- **No provider-specific test fixtures in shared packages.** The
  `eval/` directory has GitHub queries — those are dev-time
  ground-truth for the spike's hypothesis tests, not a model for
  what should land in the runtime path.

### What's allowed

Provider-specific data lives **at the data layer**, not in code:

- The `~/.config/genie/config.json` user-edited file holds per-provider
  spawn commands, URLs, scopes, OAuth client IDs.
- The OS keychain holds per-provider tokens.
- The `~/.cache/genie/crystallized/<provider>/` directory holds
  per-provider cached scripts.

These are runtime artifacts populated by the user; not code. If you
catch yourself typing a specific MCP server's name into a `.go` or
into a prompt, stop — that knowledge belongs in the catalog (the
provider tells us about itself) or in the user's config (the user
tells us which provider to use).

## Why this matters

Genie's value proposition is "drop in any MCP server, get the same
caching/shaping behavior". Provider-specific code defeats that:

- It privileges some providers over others (works great for GitHub,
  awkward for Linear).
- It rots — the provider's API changes, our prompts go stale.
- It bloats the prompt with irrelevant context for every other
  provider's calls.

The runtime tool catalog is **already** provider-specific — that's
its job. The prompts and engine just need to consume it. Anything
else is over-fitting.

## When you must touch the prompts or engine

If you're working on `internal/plan/llm.go`, `internal/engine/`, or
`internal/mcpserver/` and feel the pull to add a provider-specific
hint, the right move is one of:

1. **Augment the catalog renderer** (`renderToolCatalog`) so the
   provider's own schema better surfaces the information you'd
   otherwise hardcode. The MCP server already publishes its tool
   docs; render them more usefully.
2. **Document the abstract pattern** in the preamble. "If a tool's
   schema lists fields under `_expandable`, set the appropriate
   expand parameter" is provider-neutral and helpful.
3. **Add a config knob** (`config.ProviderConfig`) so the user
   declares the behaviour at config time, not the agent at code time.

If none of those work for your case, raise it — the right answer
might be that we shouldn't ship the feature.

## Other project conventions

- **Build/test:** `make ci` matches GitHub Actions exactly. Run it
  before pushing — your local `go build` doesn't catch the gofmt
  check or the lint pass that CI does.
- **Logging:** `slog` against `internal/logger.SetDefault()` writes
  to `~/.cache/genie/genie.log` (rotated via lumberjack). Stderr is
  reserved for top-level command failures so an MCP host's child
  pipeline isn't polluted.
- **Sessions:** every `genie serve` / `genie query` invocation gets
  a UUID JSONL log under `~/.local/share/genie/sessions/`. Tool
  calls, LLM I/O, cache events, retry events all land there. Useful
  for debugging and for offline eval.
- **Commit messages:** follow the existing style — short title,
  paragraphs explaining motivation, file:line citations when useful.
  Co-author Claude Opus.
