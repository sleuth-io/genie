package mcpclient

import (
	"context"
	"strings"

	"github.com/sleuth-io/genie/internal/runtime"
)

// HostNamePrefix is prepended to MCP tool names when they're exposed as
// monty host functions. Makes it obvious in scripts which calls leave the
// sandbox; also avoids collisions with future locally-registered helpers.
const HostNamePrefix = "github_"

// BuildHostFunctions adapts the client's tool catalog into a (BuiltIns,
// BuiltInParams) pair suitable for runtime.Capabilities. Each MCP tool
// becomes a host function callable from a monty script as
// `github_<tool_name>(arg1=..., arg2=...)`.
//
// Arguments are passed through as a kwargs map to the MCP server. We
// intentionally do NOT register positional parameter names: LLM-generated
// scripts will use kwargs (clearer + matches the JSON schema), and the few
// hand-written fixtures can do the same.
//
// Tool names are sanitized to be valid Python identifiers — MCP tool names
// can in principle contain '-' which Python would reject.
func BuildHostFunctions(c *Client) (map[string]runtime.GoFunc, map[string][]string) {
	builtIns := make(map[string]runtime.GoFunc, len(c.Tools()))
	for _, t := range c.Tools() {
		name := HostNamePrefix + sanitize(t.Name)
		builtIns[name] = makeToolFunc(c, t.Name)
	}
	return builtIns, nil
}

// MontyToolNames returns the script-side names (post-prefix, post-sanitize)
// in the same order as Tools(). Useful for dumping into prompts.
func (c *Client) MontyToolNames() []string {
	out := make([]string, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, HostNamePrefix+sanitize(t.Name))
	}
	return out
}

func makeToolFunc(c *Client, originalName string) runtime.GoFunc {
	return func(ctx context.Context, call *runtime.FunctionCall) (any, error) {
		return c.Call(ctx, originalName, call.Args)
	}
}

// sanitize converts an MCP tool name into a valid Python identifier.
// Replaces any non-alphanumeric character (other than '_') with '_'.
// Doesn't try to dedupe collisions — the spike accepts a hard error if two
// real tools collide after sanitization, which would be surprising for
// github-mcp-server.
func sanitize(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
