// Package fixtures captures upstream tool responses during the
// exploration phase of the GENERATE tool-use loop and replays them
// during script verification.
//
// The point of replay: GENERATE's tool-use loop made N real calls to
// upstream — those responses are what the LLM saw when writing the
// monty script. To verify the script behaves as the LLM expected, we
// run it against the SAME responses (rather than calling upstream
// again, which would double-bill and might return different data on
// the second call). Replay layers a fixture-aware GoFunc map over
// the real Capabilities so the script's host calls return captured
// data.
//
// Match by tool name only (per design): the script's args may differ
// from the exploration args (the user's runtime args won't match
// what the LLM picked during exploration), but the response shape is
// what matters for verification. When a tool has multiple captures,
// replay returns the most-recent one for every call to that tool —
// shape parity, not data parity.
//
// Provider-neutral: the package only knows about tool names and
// arbitrary response payloads; nothing about MCP semantics or any
// specific provider.

package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"

	"github.com/sleuth-io/genie/internal/runtime"
)

// Fixture is one captured (tool, args, response) tuple.
type Fixture struct {
	Tool     string         `json:"tool"`
	Args     map[string]any `json:"args"`
	Response any            `json:"response"`
}

// Set is the captures for one GENERATE tool-use exploration. Order
// is the order the model called them — useful for replay determinism
// and for reading the trace when debugging.
type Set []Fixture

// Append records one capture.
func (s *Set) Append(tool string, args map[string]any, response any) {
	*s = append(*s, Fixture{Tool: tool, Args: args, Response: response})
}

// LatestFor returns the most recent response captured for the named
// tool. ok=false when the tool has no capture.
func (s Set) LatestFor(tool string) (any, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].Tool == tool {
			return s[i].Response, true
		}
	}
	return nil, false
}

// MarshalJSON / UnmarshalJSON give the Set a stable on-disk shape
// when persisted alongside an L2 cache entry.
func (s Set) MarshalJSON() ([]byte, error) {
	return json.Marshal([]Fixture(s))
}

func (s *Set) UnmarshalJSON(data []byte) error {
	var raw []Fixture
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = raw
	return nil
}

// MergeFor returns a single merged response covering every capture
// for the named tool. The merge rule:
//
//   - 0 captures → (nil, false).
//   - 1 capture → the capture itself.
//   - N captures → recursive merge: lists concat, dicts deep-merge
//     (later captures' keys override on scalar conflict), other
//     types fall back to the latest.
//
// Why this matters for verification: the model's exploration phase
// often makes many small calls (resolve user A, then resolve user
// B, …) while the script the model SUBMITS may make one bigger
// call (resolve [A, B] in one batch) — or vice versa. Replay would
// give the wrong-shape result if it returned only the latest
// capture. Merging every capture for the tool name lets the
// script's calls see the union of data the model observed,
// regardless of how either side chose to batch.
func (s Set) MergeFor(tool string) (any, bool) {
	results := make([]any, 0)
	for _, f := range s {
		if f.Tool == tool {
			results = append(results, f.Response)
		}
	}
	if len(results) == 0 {
		return nil, false
	}
	if len(results) == 1 {
		return results[0], true
	}
	return mergeResults(results), true
}

// LookupFor is the args-aware replay primitive. Resolution order:
//
//  1. Exact-args match: if any capture has tool == name AND args
//     deep-equal the supplied args (compared via stable JSON
//     encoding), return that capture. This is the right answer
//     when the script's call pattern matches the model's
//     exploration — both resolved users one-at-a-time, both used
//     the same JQL, etc.
//  2. Merged fallback: when the script batches differently from
//     exploration (script: one batched call; model: multiple
//     small calls — or vice versa), return MergeFor's deep-merge
//     so the script sees the union of observed data.
//  3. Miss: (nil, false) when the tool has no captures at all.
func (s Set) LookupFor(tool string, args map[string]any) (any, bool) {
	wanted, _ := json.Marshal(args)
	for _, f := range s {
		if f.Tool != tool {
			continue
		}
		got, _ := json.Marshal(f.Args)
		if string(got) == string(wanted) {
			slog.Debug("fixtures: exact match", "tool", tool, "args_len", len(string(wanted)))
			return f.Response, true
		}
	}
	slog.Warn("fixtures: no exact-args match, falling back to merge",
		"tool", tool,
		"wanted", truncStr(string(wanted), 200),
	)
	return s.MergeFor(tool)
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func mergeResults(results []any) any {
	if len(results) == 0 {
		return nil
	}
	if len(results) == 1 {
		return results[0]
	}
	allLists := true
	for _, r := range results {
		if _, ok := r.([]any); !ok {
			allLists = false
			break
		}
	}
	if allLists {
		var out []any
		for _, r := range results {
			out = append(out, r.([]any)...)
		}
		return out
	}
	allDicts := true
	for _, r := range results {
		if _, ok := r.(map[string]any); !ok {
			allDicts = false
			break
		}
	}
	if allDicts {
		out := map[string]any{}
		for _, r := range results {
			for k, v := range r.(map[string]any) {
				if existing, present := out[k]; present {
					out[k] = mergeResults([]any{existing, v})
				} else {
					out[k] = v
				}
			}
		}
		return out
	}
	return results[len(results)-1]
}

// ReplayCapabilities returns a copy of caps whose BuiltIns are
// wrapped to consult the fixture set first.
//
// Three behaviours per BuiltIn name:
//   - captured: static replay returns the most-recent observed response.
//   - upstream tool (name in upstreamTools) but no capture: error. The
//     script tried to call a tool the LLM didn't explore — verification
//     would otherwise fall through to a real upstream call (slow,
//     unauthenticated, possibly mutating). Instead surface the gap so
//     the LLM can revise.
//   - everything else (clock helpers, other host builtins): pass through
//     to the real implementation. Pure host code is safe to run at
//     verification time.
//
// The original caps is not modified.
func ReplayCapabilities(caps *runtime.Capabilities, set Set, upstreamTools []string) *runtime.Capabilities {
	if caps == nil {
		return nil
	}
	upstream := make(map[string]struct{}, len(upstreamTools))
	for _, n := range upstreamTools {
		upstream[n] = struct{}{}
	}
	hasCaptureFor := func(tool string) bool {
		for _, f := range set {
			if f.Tool == tool {
				return true
			}
		}
		return false
	}

	wrapped := make(map[string]runtime.GoFunc, len(caps.BuiltIns))
	for name := range caps.BuiltIns {
		fn := caps.BuiltIns[name]
		if hasCaptureFor(name) {
			toolName := name
			wrapped[name] = func(_ context.Context, call *runtime.FunctionCall) (any, error) {
				if resp, ok := set.LookupFor(toolName, call.Args); ok {
					return resp, nil
				}
				return nil, fmt.Errorf("verification: no fixture for %q", toolName)
			}
			continue
		}
		if _, isUpstream := upstream[name]; isUpstream {
			wrapped[name] = unexploredErr(name)
			continue
		}
		wrapped[name] = fn
	}

	out := *caps
	out.BuiltIns = wrapped
	return &out
}

func unexploredErr(name string) runtime.GoFunc {
	return func(_ context.Context, _ *runtime.FunctionCall) (any, error) {
		return nil, fmt.Errorf("verification: script called %q but no fixture was captured for it; explore that tool before submitting the script", name)
	}
}

// CaptureWrap wraps a single GoFunc so that every successful call
// records a fixture into dst. Used when assembling the host-function
// bundle for the exploration phase: each MCP-tool wrapper is
// CaptureWrap'd so the loop driver collects fixtures without
// special-casing which tool was called.
func CaptureWrap(toolName string, inner runtime.GoFunc, dst *Set) runtime.GoFunc {
	if dst == nil {
		return inner
	}
	return func(ctx context.Context, call *runtime.FunctionCall) (any, error) {
		response, err := inner(ctx, call)
		if err != nil {
			return response, err
		}
		args := map[string]any{}
		for k, v := range call.Args {
			args[k] = v
		}
		dst.Append(toolName, args, response)
		return response, nil
	}
}

// Diff compares two values for verification matching. Returns "" if
// they match per the rules below, otherwise a human-readable
// description of the first divergence (suitable for feeding back to
// the LLM as a revision hint).
//
// Match rules:
//   - For maps: same keys, recursive match on values. Extra keys in
//     `actual` not in `expected` are NOT a divergence — the LLM may
//     have written expected_output covering only the keys it cares
//     about, and the script is allowed to return more.
//   - For lists: same length OR expected truncated by the LLM. We
//     compare only the first len(expected) elements of actual.
//   - For scalars: equality, with two leniencies:
//   - nil and "" are interchangeable (script may default a missing
//     string to None or empty).
//   - 0/false don't equal nil — those are real signals.
//
// The expected side is what the LLM CLAIMED the output would be; the
// actual side is what the script PRODUCED. Asymmetry is intentional.
func Diff(expected, actual any, path string) string {
	switch e := expected.(type) {
	case map[string]any:
		am, ok := actual.(map[string]any)
		if !ok {
			return fmt.Sprintf("at %s: expected object, got %T (%v)", pathOrRoot(path), actual, actual)
		}
		for k, ev := range e {
			av, present := am[k]
			if !present {
				return fmt.Sprintf("at %s.%s: expected key missing in actual", pathOrRoot(path), k)
			}
			if d := Diff(ev, av, path+"."+k); d != "" {
				return d
			}
		}
		return ""
	case []any:
		al, ok := actual.([]any)
		if !ok {
			return fmt.Sprintf("at %s: expected list, got %T", pathOrRoot(path), actual)
		}
		// Allow actual to be longer than expected (LLM may have
		// truncated for brevity) but not shorter (would mean the
		// script lost rows).
		if len(al) < len(e) {
			return fmt.Sprintf("at %s: expected at least %d elements, got %d", pathOrRoot(path), len(e), len(al))
		}
		for i, ev := range e {
			if d := Diff(ev, al[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	case nil:
		if actual == nil {
			return ""
		}
		if s, ok := actual.(string); ok && s == "" {
			return ""
		}
		return fmt.Sprintf("at %s: expected null, got %v", pathOrRoot(path), actual)
	case string:
		if e == "" && actual == nil {
			return ""
		}
		if a, ok := actual.(string); ok && a == e {
			return ""
		}
		return fmt.Sprintf("at %s: expected %q, got %v", pathOrRoot(path), e, actual)
	default:
		// Numeric scalars: relative-tolerance compare so that
		// float-arithmetic ordering drift (e.g. 9.313641864960575 vs
		// 9.313641864960577, one ULP apart) doesn't read as a
		// divergence. The threshold is tight enough that any real
		// script bug (>0.0001%) still trips it. Integers go through
		// here too, exactly because float64(int) is exact for small
		// magnitudes.
		if ef, eok := numericValue(expected); eok {
			if af, aok := numericValue(actual); aok {
				if floatsClose(ef, af) {
					return ""
				}
				return fmt.Sprintf("at %s: expected %v, got %v", pathOrRoot(path), expected, actual)
			}
		}
		// Booleans, or numeric-vs-non-numeric — fall back to fmt-equality.
		if fmt.Sprintf("%v", expected) == fmt.Sprintf("%v", actual) {
			return ""
		}
		return fmt.Sprintf("at %s: expected %v, got %v", pathOrRoot(path), expected, actual)
	}
}

// numericValue extracts a float64 from the numeric Go types JSON
// round-trip can produce (and json.Number for streaming decoders).
// Returns (0, false) for non-numeric values.
func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
		return 0, false
	}
	return 0, false
}

// floatsClose reports whether a and b are equal for verify purposes.
// Combined tolerance: an absolute floor of 1e-12 covers near-zero
// comparisons; a relative threshold of 1e-9 absorbs float-arithmetic
// ordering noise (one-ULP drift is ~2e-16 — well under 1e-9) without
// masking real script bugs (e.g. a 0.6% diff is 6e-3, far above).
func floatsClose(a, b float64) bool {
	if a == b {
		return true
	}
	diff := math.Abs(a - b)
	if diff <= 1e-12 {
		return true
	}
	return diff <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}

func pathOrRoot(p string) string {
	if p == "" {
		return "<root>"
	}
	return p
}
