// Package mcpserver exposes Genie's resolution pipeline over MCP stdio.
// It registers two tools: run_query (the headline tool — resolves a
// GraphQL-shaped query against one of the fronted providers) and
// list_providers (returns the configured provider names + descriptions
// so the calling agent knows what's wired up).
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/sleuth-io/genie/internal/progress"
	"github.com/sleuth-io/genie/internal/providers"
)

// QueryRunner resolves a query against a named provider. The implementation
// is responsible for looking the provider up; an unknown name should
// surface as a normal Go error.
type QueryRunner func(ctx context.Context, provider, query string) (map[string]any, error)

// ProviderLister returns the live provider catalogue the MCP tools
// describe. Decoupled from a concrete *providers.Registry so tests can
// supply their own.
type ProviderLister interface {
	Names() []string
	List() []providers.Info
}

// NewServer builds an MCPServer with run_query and list_providers
// registered. `version` flows into the MCP server-info handshake.
func NewServer(version string, runner QueryRunner, lister ProviderLister) *server.MCPServer {
	s := server.NewMCPServer("genie", version,
		server.WithToolCapabilities(false),
	)
	s.AddTool(runQueryTool(lister), runQueryHandler(s, runner, lister))
	s.AddTool(listProvidersTool(), listProvidersHandler(lister))
	return s
}

func runQueryTool(lister ProviderLister) mcp.Tool {
	names := append([]string(nil), lister.Names()...)
	sort.Strings(names)

	properties := map[string]any{
		"provider": map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Target provider. Configured: %s.", strings.Join(names, ", ")),
		},
		"query": map[string]any{
			"type":        "string",
			"description": "A GraphQL-shaped query string. Field names may be invented — they are treated as intent signals, not schema references. Example: `{ pull_requests(owner: \"x\", repo: \"y\", state: \"open\") { title number author { login } } }`.",
		},
	}
	if len(names) > 0 {
		properties["provider"].(map[string]any)["enum"] = names
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"provider", "query"},
	}
	raw, _ := json.Marshal(schema)
	return mcp.Tool{
		Name:           "run_query",
		Description:    "Resolve a GraphQL-shaped query against the named provider. The first call for a given query shape pays an LLM-generation cost; subsequent calls hit the crystallized cache. Returns JSON whose top-level shape matches the requested field selection.",
		RawInputSchema: raw,
	}
}

func runQueryHandler(srv *server.MCPServer, runner QueryRunner, lister ProviderLister) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		provider, _ := args["provider"].(string)
		if provider == "" {
			return mcp.NewToolResultError("missing required arg: provider"), nil
		}
		if !contains(lister.Names(), provider) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"provider %q not configured; known: %s",
				provider, strings.Join(lister.Names(), ", "),
			)), nil
		}

		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("missing required arg: query"), nil
		}

		ctx = withProgressFromRequest(ctx, srv, req)

		result, err := runner(ctx, provider, query)
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

// withProgressFromRequest wires a progress.Sender into ctx when the
// caller opted into progress notifications via _meta.progressToken.
// Each Sender call ships one notifications/progress message with an
// elapsed-time prefix and a monotonically-increasing progress
// counter.
//
// Returns ctx unchanged when the client didn't provide a token —
// progress.Report is a no-op in that case, so call sites need not
// branch.
func withProgressFromRequest(ctx context.Context, srv *server.MCPServer, req mcp.CallToolRequest) context.Context {
	if req.Params.Meta == nil || req.Params.Meta.ProgressToken == nil {
		return ctx
	}
	token := req.Params.Meta.ProgressToken
	start := time.Now()
	var counter atomic.Int64
	sender := func(msg string) {
		counter.Add(1)
		elapsed := time.Since(start).Seconds()
		params := map[string]any{
			"progressToken": token,
			"progress":      float64(counter.Load()),
			"message":       fmt.Sprintf("[%.1fs] %s", elapsed, msg),
		}
		if err := srv.SendNotificationToClient(ctx, "notifications/progress", params); err != nil {
			slog.Debug("progress send failed", "err", err)
		}
	}
	return progress.WithSender(ctx, sender)
}

func listProvidersTool() mcp.Tool {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	raw, _ := json.Marshal(schema)
	return mcp.Tool{
		Name:           "list_providers",
		Description:    "List the providers (upstream MCP servers) that Genie is fronting. Returns name + description for each. Tool catalogues are intentionally not exposed — that would defeat the purpose of routing through Genie.",
		RawInputSchema: raw,
	}
}

func listProvidersHandler(lister ProviderLister) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		buf, err := json.MarshalIndent(lister.List(), "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		return mcp.NewToolResultText(string(buf)), nil
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
