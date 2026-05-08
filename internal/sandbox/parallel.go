// Parallel host helper. Lets monty scripts fan out independent host
// calls concurrently rather than walking them serially. Designed for
// the in-loop ID-resolution pattern: a script iterating N pages and
// calling lookupX(authorId) once per page goes from N×~400ms wall
// to ~400ms total once batched through `parallel`.
//
// Script-side shape (stable, expected by the GENERATE prompt):
//
//     results = parallel([
//         {"fn": "host_function_name", "args": {"key": "value1"}},
//         {"fn": "host_function_name", "args": {"key": "value2"}},
//     ])
//     # results[i] is {"ok": <return-value>} or {"error": "<msg>"}.
//     # Same length + order as input.
//
// Optional second arg: `parallel(calls, max_concurrency=8)`.
//
// Provider-neutral by design: takes ANY host function name in the
// caps map, dispatches via the same wrapped GoFunc the script would
// invoke directly. Session recording, error propagation, and ctx
// cancellation all flow through the underlying function unchanged.

package sandbox

import (
	"context"
	"fmt"
	"sync"

	"github.com/sleuth-io/genie/internal/runtime"
)

// defaultMaxConcurrency caps how many host calls fire at once.
// Small enough to avoid rate-limiting an upstream MCP server, large
// enough to win on the typical 5-10 element fan-out. Override per
// call via the parallel(_, max_concurrency=N) kwarg.
const defaultMaxConcurrency = 8

// BuildParallelBuiltins registers the `parallel` builtin. It needs a
// resolver closure because the dispatch table is built AFTER the
// other bundles are merged — passing a snapshot of the caps map
// would miss tool-wrapped GoFuncs that get registered later. The
// caller threads the post-merge map in as `lookup`.
func BuildParallelBuiltins(lookup func(name string) (runtime.GoFunc, bool)) (map[string]runtime.GoFunc, map[string][]string) {
	fn := func(ctx context.Context, call *runtime.FunctionCall) (any, error) {
		callsArg, ok := call.Args["calls"]
		if !ok {
			return nil, fmt.Errorf("parallel: missing required arg 'calls'")
		}
		calls, ok := callsArg.([]any)
		if !ok {
			return nil, fmt.Errorf("parallel: 'calls' must be a list, got %T", callsArg)
		}

		max := defaultMaxConcurrency
		if v, present := call.Args["max_concurrency"]; present {
			switch t := v.(type) {
			case float64:
				if t > 0 {
					max = int(t)
				}
			case int:
				if t > 0 {
					max = t
				}
			case int64:
				if t > 0 {
					max = int(t)
				}
			}
		}
		if max < 1 {
			max = 1
		}

		results := make([]any, len(calls))
		sem := make(chan struct{}, max)
		var wg sync.WaitGroup

		for i, raw := range calls {
			spec, ok := raw.(map[string]any)
			if !ok {
				results[i] = map[string]any{"error": fmt.Sprintf("entry %d not a dict", i)}
				continue
			}
			name, _ := spec["fn"].(string)
			if name == "" {
				results[i] = map[string]any{"error": fmt.Sprintf("entry %d missing 'fn'", i)}
				continue
			}
			if name == "parallel" {
				results[i] = map[string]any{"error": "parallel cannot call itself"}
				continue
			}
			args, _ := spec["args"].(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			target, found := lookup(name)
			if !found {
				results[i] = map[string]any{"error": fmt.Sprintf("unknown host function %q", name)}
				continue
			}

			wg.Add(1)
			go func(idx int, name string, args map[string]any, target runtime.GoFunc) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					results[idx] = map[string]any{"error": ctx.Err().Error()}
					return
				}
				defer func() { <-sem }()

				if ctx.Err() != nil {
					results[idx] = map[string]any{"error": ctx.Err().Error()}
					return
				}
				inner := &runtime.FunctionCall{Name: name, Args: args}
				v, err := target(ctx, inner)
				if err != nil {
					results[idx] = map[string]any{"error": err.Error()}
					return
				}
				results[idx] = map[string]any{"ok": v}
			}(i, name, args, target)
		}
		wg.Wait()
		return results, nil
	}

	return map[string]runtime.GoFunc{
			"parallel": fn,
		}, map[string][]string{
			"parallel": {"calls", "max_concurrency"},
		}
}
