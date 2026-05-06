// Package sandbox holds host-function bundles that extend the monty
// runtime's surface. These are deliberately small, deliberately safe, and
// deliberately named so the LLM can use them naturally.
//
// Clock helpers fill the most common gap exposed by Day-5 hypothesis-1
// failures: the monty sandbox has no `datetime.now()` (the WASM guest has no
// system clock and OS-call passthrough isn't wired). Asking the LLM to
// "never compute the current time" worked but produced awkward code.
// Exposing `now_iso`, `now_epoch`, and `days_ago_iso` lets it write the
// idiom it actually wants.
package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/sleuth-io/genie/internal/runtime"
)

// BuildClockBuiltins returns the (BuiltIns, BuiltInParams) pair to merge
// into runtime.Capabilities. All times are UTC; ISO strings use RFC 3339
// (e.g. "2026-05-06T14:30:00Z").
func BuildClockBuiltins() (map[string]runtime.GoFunc, map[string][]string) {
	builtIns := map[string]runtime.GoFunc{
		"now_iso":       nowISO,
		"now_epoch":     nowEpoch,
		"days_ago_iso":  daysAgoISO,
		"hours_ago_iso": hoursAgoISO,
	}
	params := map[string][]string{
		"days_ago_iso":  {"n"},
		"hours_ago_iso": {"n"},
	}
	return builtIns, params
}

func nowISO(_ context.Context, _ *runtime.FunctionCall) (any, error) {
	return time.Now().UTC().Format(time.RFC3339), nil
}

func nowEpoch(_ context.Context, _ *runtime.FunctionCall) (any, error) {
	return time.Now().UTC().Unix(), nil
}

func daysAgoISO(_ context.Context, call *runtime.FunctionCall) (any, error) {
	n, err := intArg(call, "n")
	if err != nil {
		return nil, err
	}
	return time.Now().UTC().AddDate(0, 0, -n).Format(time.RFC3339), nil
}

func hoursAgoISO(_ context.Context, call *runtime.FunctionCall) (any, error) {
	n, err := intArg(call, "n")
	if err != nil {
		return nil, err
	}
	return time.Now().UTC().Add(-time.Duration(n) * time.Hour).Format(time.RFC3339), nil
}

// intArg coerces the named arg from a FunctionCall payload. Numeric
// arguments arrive as float64 because they survive the JSON round-trip;
// integer literals serialize as floats. We accept either and clamp to int.
func intArg(call *runtime.FunctionCall, name string) (int, error) {
	v, ok := call.Args[name]
	if !ok {
		return 0, fmt.Errorf("%s: missing required arg %q", call.Name, name)
	}
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case int:
		return t, nil
	case int64:
		return int(t), nil
	}
	return 0, fmt.Errorf("%s: arg %q must be a number, got %T", call.Name, name, v)
}

// MergeBuiltins is a small helper for combining bundles (clock + MCP tools).
// Returns new maps; does not mutate inputs. Later bundles win on key
// conflicts — caller's responsibility to avoid overlaps.
func MergeBuiltins(
	bundles ...struct {
		Funcs  map[string]runtime.GoFunc
		Params map[string][]string
	},
) (map[string]runtime.GoFunc, map[string][]string) {
	funcs := map[string]runtime.GoFunc{}
	params := map[string][]string{}
	for _, b := range bundles {
		for k, v := range b.Funcs {
			funcs[k] = v
		}
		for k, v := range b.Params {
			params[k] = v
		}
	}
	return funcs, params
}
