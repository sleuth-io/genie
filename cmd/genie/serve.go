package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"github.com/sleuth-io/genie/internal/buildinfo"
	"github.com/sleuth-io/genie/internal/mcpserver"
	"github.com/sleuth-io/genie/pkg/genie"
)

// runServe boots the resolution pipeline and exposes it as MCP tools —
// run_query and list_providers — over stdio. Connect from Claude
// Desktop, Claude Code, mcp-inspector, or any MCP client.
//
// stdio is the only transport for v1. HTTP/SSE comes later.
func runServe(ctx context.Context, _ []string) error {
	g, err := genie.New(ctx, genie.Config{
		AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
	})
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()

	runner := func(ctx context.Context, provider, query string) (map[string]any, error) {
		return g.QueryMap(ctx, provider, query)
	}

	srv := mcpserver.NewServer(buildinfo.Version, runner, g)
	slog.Info("genie serve: stdio MCP server ready",
		"providers", g.ProviderNames())

	if err := server.ServeStdio(srv); err != nil {
		return fmt.Errorf("serve stdio: %w", err)
	}
	return nil
}
