// Package mcpserver exposes the spike's resolution pipeline as a single
// MCP tool — run_query(provider, query) — over stdio. This is the eventual
// product surface; for the spike it lets Claude Desktop, mcp-inspector, or
// any other MCP client drive the same engine the CLI uses.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// QueryRunner is the minimal contract the tool needs: parse a GraphQL
// string + execute, returning a `{aliasOrName: value, ...}` map. The
// caller wires this to the live engine.Executor in cmd/intent-gw/serve.go.
type QueryRunner func(ctx context.Context, query string) (map[string]any, error)

// SupportedProviders are the values accepted for the `provider` arg today.
// Spike scope is GitHub-only; multi-provider routing lands later.
var SupportedProviders = []string{"github"}

// NewServer builds an MCPServer with one registered tool, run_query.
// `version` is reflected in MCP server-info responses; pass something like
// "spike" or a build SHA.
func NewServer(version string, runner QueryRunner) *server.MCPServer {
	s := server.NewMCPServer("intent-gateway", version,
		server.WithToolCapabilities(false),
	)
	s.AddTool(runQueryTool(), runQueryHandler(runner))
	return s
}

// runQueryTool defines the tool's metadata + JSON-Schema input. Kept as a
// constructor for testability — the same shape is exposed to clients.
func runQueryTool() mcp.Tool {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Target system. Currently supported: %s.", strings.Join(SupportedProviders, ", ")),
				"enum":        SupportedProviders,
			},
			"query": map[string]any{
				"type":        "string",
				"description": "A GraphQL-shaped query string. Field names may be invented — they are treated as intent signals, not schema references. Example: `{ pull_requests(owner: \"x\", repo: \"y\", state: \"open\") { title number author { login } } }`.",
			},
		},
		"required": []string{"provider", "query"},
	}
	raw, _ := json.Marshal(schema)
	return mcp.Tool{
		Name:        "run_query",
		Description: "Resolve a hallucinated GraphQL-shaped query against the target system. The first call for a given query shape pays an LLM-generation cost; subsequent calls hit the crystallized cache. Returns JSON whose top-level shape matches the requested field selection.",
		RawInputSchema: raw,
	}
}

// runQueryHandler is the per-call handler. Validates `provider`, defers to
// `runner`, and serialises the result as a single text block. Errors come
// back as MCP tool-error results so the calling LLM can self-correct.
func runQueryHandler(runner QueryRunner) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		provider, _ := args["provider"].(string)
		if provider == "" {
			return mcp.NewToolResultError("missing required arg: provider"), nil
		}
		if !providerSupported(provider) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"provider %q not supported; supported: %s",
				provider, strings.Join(SupportedProviders, ", "),
			)), nil
		}

		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("missing required arg: query"), nil
		}

		result, err := runner(ctx, query)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("resolution failed: %v", err)), nil
		}

		buf, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		return mcp.NewToolResultText(string(buf)), nil
	}
}

func providerSupported(p string) bool {
	for _, s := range SupportedProviders {
		if s == p {
			return true
		}
	}
	return false
}
