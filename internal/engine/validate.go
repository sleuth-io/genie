package engine

import "regexp"

// Forbidden script patterns. The GENERATE prompt tells the LLM to
// let upstream errors propagate so the executor's retry loop can
// catch them — try/except wrappers silently swallow real errors and
// turn them into empty results, defeating the retry path entirely.
//
// We enforce the rule at script-validation time rather than relying
// on the prompt alone. If a forbidden pattern matches, the executor
// triggers Regenerate with the violation as the error message; the
// LLM gets one or more chances to write a compliant script before
// the call fails.
//
// Patterns target Python keywords syntactically. The regex `\btry\s*:`
// matches a try-block start regardless of context — there's no
// legitimate try block in a Genie-generated script: defensive type
// handling should use isinstance() checks and dict.get(default)
// fallbacks, not try/except.
//
// This is a generic Python concern (no provider-specific semantics),
// so it lives in the engine package alongside the other static
// invariants the runtime enforces.
var forbiddenScriptPatterns = []struct {
	rx     *regexp.Regexp
	reason string
}{
	{
		rx:     regexp.MustCompile(`\btry\s*:`),
		reason: "script contains a `try:` block — the GENERATE prompt forbids this. Errors must propagate so the executor's retry loop sees them. Use explicit isinstance() and dict.get(default) checks instead.",
	},
	{
		rx:     regexp.MustCompile(`\bexcept\b`),
		reason: "script contains an `except` clause — the GENERATE prompt forbids try/except. Errors must propagate so the executor's retry loop sees them. Use explicit isinstance() and dict.get(default) checks instead.",
	},
}

// ValidateScript checks the generated script against pre-execution
// invariants. Returns "" if the script is acceptable, otherwise a
// human-readable violation that the caller should pass to Regenerate
// as the error context for the next attempt.
func ValidateScript(src string) string {
	for _, p := range forbiddenScriptPatterns {
		if p.rx.MatchString(src) {
			return p.reason
		}
	}
	return ""
}
