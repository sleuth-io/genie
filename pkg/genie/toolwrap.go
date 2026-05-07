package genie

import (
	"context"
	"encoding/json"
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
//   - result size in bytes (cheap proxy for "how chunky was this
//     response" — useful for runtime evals on response shaping)
//   - duration
//   - error if any
//
// We deliberately don't store the full tool result. MCP responses
// can run thousands of tokens; bloating the session log with them
// would defeat the value (the SHAPED result is what gets returned to
// the agent's LLM, and the script's logic on top of the raw result
// is captured in the GENERATE record's `response` field).
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
		} else if result != nil {
			if buf, marshalErr := json.Marshal(result); marshalErr == nil {
				rec.ResultBytes = len(buf)
			}
		}
		session.FromContext(ctx).AppendCtx(ctx, rec)
		return result, err
	}
}
