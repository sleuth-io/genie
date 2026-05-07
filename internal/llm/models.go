package llm

// Canonical Claude model IDs Genie ships with. Any reference to a
// specific model — defaults, README docs, env-var examples — should
// route through these constants so a model bump is one change.
//
// Verified against github.com/anthropics/anthropic-sdk-go v1.41.0:
// the SDK exposes the same strings as ModelClaudeOpus4_7,
// ModelClaudeSonnet4_6, ModelClaudeHaiku4_5. We don't reference the
// SDK constants directly because the Claude Code CLI also accepts
// these strings as `--model` arguments — keeping a single source of
// truth lets both backends use one identifier.
//
// When new models ship, update these (and the README's listing) in
// one PR.
const (
	// ModelOpus is the latest Opus — best for GENERATE (writes
	// monty scripts that have to reason about MCP tool schemas).
	ModelOpus = "claude-opus-4-7"

	// ModelSonnet is the latest Sonnet — fast, cheap, plenty for
	// most plan generation. Good NORMALIZE choice; usually fine
	// for GENERATE too on simpler queries.
	ModelSonnet = "claude-sonnet-4-6"

	// ModelHaiku is the smallest current model — best for
	// NORMALIZE only (canonical-schema output is short and
	// structured). May trade off some paraphrase-collision
	// fidelity; gate behind the eval/adversarial.yaml set before
	// committing to it as a default.
	ModelHaiku = "claude-haiku-4-5"

	// DefaultModel is the fallback when no GENIE_*_MODEL env var
	// is set and no per-call ctx override is attached. Errs on the
	// side of correctness over speed.
	DefaultModel = ModelOpus
)
