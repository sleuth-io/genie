package genie

import (
	"context"
	"time"

	"github.com/sleuth-io/genie/internal/runtime"
	"github.com/sleuth-io/genie/internal/session"
)

// wrapToolFunc returns a runtime.GoFunc that records each tool call
// to the session attached to ctx (if any) and otherwise behaves
// identically to inner.
//
// What goes into the record:
//   - tool name (the host function the script invoked, e.g.
//     "github_list_pull_requests")
//   - args (the kwargs the script passed)
//   - the full upstream tool result — raw API data the monty script
//     consumes; never seen by the calling agent unmodified. Capturing
//     it lets a session reader see what input the script reasoned
//     against, which is the load-bearing thing for runtime eval
//   - duration
//   - error if any
func wrapToolFunc(provider, fnName string, inner runtime.GoFunc) runtime.GoFunc {
	return func(ctx context.Context, call *runtime.FunctionCall) (any, error) {
		start := time.Now()
		result, err := inner(ctx, call)

		rec := session.Record{
			Call:       "tool_call",
			Provider:   provider,
			Tool:       call.Name,
			ToolArgs:   call.Args,
			DurationMS: time.Since(start).Milliseconds(),
		}
		if err != nil {
			rec.Err = err.Error()
		} else {
			rec.Result = result
		}
		session.FromContext(ctx).AppendCtx(ctx, rec)
		return result, err
	}
}
