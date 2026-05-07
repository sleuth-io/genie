package eval

import (
	"fmt"
	"strings"
	"time"

	"github.com/sleuth-io/genie/internal/session"
)

// Check applies all assertions in `a` against `result`. Returns nil on
// pass, or a single error summarising the first failure.
//
// Path syntax is dot-delimited keys, e.g. `viewer.login` or
// `repository.owner.name`. There's no list-element syntax in plain paths
// — list iteration is handled by NonemptyPathsInEach.
func (a Assertions) Check(result map[string]any) error {
	for _, p := range a.ListPaths {
		v, ok := lookup(result, p)
		if !ok {
			return fmt.Errorf("list_paths: %q not present", p)
		}
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("list_paths: %q is %T, want list", p, v)
		}
		if len(list) == 0 {
			return fmt.Errorf("list_paths: %q is empty", p)
		}
	}

	for _, p := range a.NonemptyPaths {
		v, ok := lookup(result, p)
		if !ok {
			return fmt.Errorf("nonempty_paths: %q not present", p)
		}
		if isEmpty(v) {
			return fmt.Errorf("nonempty_paths: %q is empty (%v)", p, v)
		}
	}

	for listPath, fields := range a.NonemptyPathsInEach {
		v, ok := lookup(result, listPath)
		if !ok {
			return fmt.Errorf("nonempty_paths_in_each: list %q not present", listPath)
		}
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("nonempty_paths_in_each: %q is %T, want list", listPath, v)
		}
		if len(list) == 0 {
			return fmt.Errorf("nonempty_paths_in_each: %q is empty list", listPath)
		}
		for i, item := range list {
			obj, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("nonempty_paths_in_each: %q[%d] is %T, want object", listPath, i, item)
			}
			for _, field := range fields {
				v, ok := lookup(obj, field)
				if !ok {
					return fmt.Errorf("nonempty_paths_in_each: %q[%d].%s not present", listPath, i, field)
				}
				if isEmpty(v) {
					return fmt.Errorf("nonempty_paths_in_each: %q[%d].%s is empty (%v)", listPath, i, field, v)
				}
			}
		}
	}
	return nil
}

// lookup walks dot-delimited path segments through nested maps. Returns
// (zero, false) if any segment is missing or any intermediate is not a map.
func lookup(root map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = root
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := m[p]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// CheckBehavior runs the behavioural assertions against a slice of
// session records (the events produced during the scenario's query)
// and the measured wall duration. Independent of Check (the result-
// shape pass) — both must pass for the scenario to PASS overall.
func (a Assertions) CheckBehavior(records []session.Record, duration time.Duration) error {
	if a.MaxDurationMS > 0 && duration.Milliseconds() > a.MaxDurationMS {
		return fmt.Errorf("max_duration_ms: query took %dms, budget %dms", duration.Milliseconds(), a.MaxDurationMS)
	}

	if a.MaxLLMCalls > 0 {
		count := 0
		for _, r := range records {
			switch r.Call {
			case "normalize", "generate", "regenerate":
				count++
			}
		}
		if count > a.MaxLLMCalls {
			return fmt.Errorf("max_llm_calls: %d normalize+generate+regenerate events, budget %d", count, a.MaxLLMCalls)
		}
	}

	for _, pat := range a.ForbidInScripts {
		for _, r := range records {
			if r.Call != "generate" && r.Call != "regenerate" {
				continue
			}
			if strings.Contains(r.Response, pat) {
				return fmt.Errorf("forbid_in_scripts: %q appears in %s output for field %q", pat, r.Call, r.Field)
			}
		}
	}

	for _, want := range a.ExpectSynthesize {
		// Pass if no fresh GENERATE fired for this field. A
		// synthesize event, an L1 cache hit, or an L2 cache hit on
		// an earlier synthesize all qualify — they're indistin-
		// guishable from the user's perspective and they all skip
		// the LLM cost. A fresh GENERATE is the only failure mode
		// (the synthesize gate let one through).
		for _, r := range records {
			if r.Call == "generate" && r.Field == want {
				return fmt.Errorf("expect_synthesize_for: GENERATE fired for %q (synthesize gate let it through, paid LLM cost)", want)
			}
		}
	}
	return nil
}

// isEmpty matches our notion of "missing data" for assertions: nil,
// empty string, empty list, empty map, or numeric zero. Booleans count
// as non-empty regardless of value (false is a valid signal).
func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case float64:
		return t == 0
	case int:
		return t == 0
	case bool:
		return false
	}
	return false
}
