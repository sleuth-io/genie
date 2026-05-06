package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/server"

	"github.com/mrdon/gqlspike/internal/engine"
	"github.com/mrdon/gqlspike/internal/mcpserver"
)

// runServe boots the resolution pipeline and exposes it as a single MCP
// tool — run_query(provider, query) — over stdio. Connect from Claude
// Desktop, mcp-inspector, or any MCP client.
//
// stdio is the only transport for the spike. HTTP/SSE comes later if we
// productionize.
func runServe(ctx context.Context, _ []string) error {
	bundle, err := setupEngine(ctx, "./crystallized")
	if err != nil {
		return err
	}
	defer bundle.Close()

	runner := func(ctx context.Context, query string) (map[string]any, error) {
		parsed, err := engine.Parse(query)
		if err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		return bundle.executor.Execute(ctx, parsed)
	}

	srv := mcpserver.NewServer("spike", runner)
	slog.Info("intent-gw serve: stdio MCP server ready",
		"tool", "run_query", "providers", mcpserver.SupportedProviders)

	if err := server.ServeStdio(srv); err != nil {
		return fmt.Errorf("serve stdio: %w", err)
	}
	return nil
}
