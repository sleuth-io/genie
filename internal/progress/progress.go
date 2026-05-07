// Package progress is a tiny ctx-plumbing layer for surfacing
// status messages to the calling MCP host during a long tool call.
//
// MCP's `notifications/progress` channel is the right transport
// for this — it's request-scoped, side-band, and doesn't pollute
// the final tool response. mcpserver/runquery.go pulls the
// progressToken off the incoming request, builds a Sender that
// formats + ships notifications, and attaches it to ctx via
// WithSender. Downstream call sites (plan generator, tool-call
// wrapper, Genie.Query) call Report and don't have to know
// anything about MCP transport.
//
// Each Report includes elapsed time since the Sender was created
// so the user sees both "what's happening" and "how long this is
// taking" without doing mental math.
package progress

import (
	"context"
	"fmt"
)

type senderKey struct{}

// Sender ships one progress message to the client. The
// implementation owns formatting, monotonic counters, and the
// underlying transport call. nil-safe — Report is a no-op when
// no sender is attached to ctx.
type Sender func(message string)

// WithSender attaches a Sender to ctx. Returns the unmodified ctx
// when fn is nil so callers don't have to branch.
func WithSender(ctx context.Context, fn Sender) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, senderKey{}, fn)
}

// FromContext returns the Sender attached to ctx, or nil.
func FromContext(ctx context.Context) Sender {
	if v, ok := ctx.Value(senderKey{}).(Sender); ok {
		return v
	}
	return nil
}

// Report fires one progress message via the Sender attached to
// ctx. No-op when no sender. Thin wrapper around fmt.Sprintf so
// call sites read naturally.
func Report(ctx context.Context, format string, args ...any) {
	fn := FromContext(ctx)
	if fn == nil {
		return
	}
	if len(args) == 0 {
		fn(format)
		return
	}
	fn(fmt.Sprintf(format, args...))
}
