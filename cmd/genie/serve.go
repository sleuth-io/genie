package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/server"

	"github.com/sleuth-io/genie/internal/buildinfo"
	"github.com/sleuth-io/genie/internal/envfile"
	"github.com/sleuth-io/genie/internal/mcpserver"
	"github.com/sleuth-io/genie/pkg/genie"
)

// loadProjectEnv hunts for a .env file in CWD, the binary's dir,
// and one level up (catches the common dist/<binary>-in-repo
// layout). First match wins; existing process env always
// out-prioritises file contents (see envfile.LoadPath).
//
// Also maps SLEUTH_CLAUDE_API_KEY → ANTHROPIC_API_KEY when the
// latter isn't already set, so genie's SDK backend lights up from
// a project's existing secret. Useful when the MCP launcher
// (claude-code etc.) doesn't propagate envs and a wrapper script
// is awkward.
func loadProjectEnv() {
	candidates := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, ".env"))
		candidates = append(candidates, filepath.Join(filepath.Dir(dir), ".env"))
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := envfile.LoadPath(path); err != nil {
			slog.Warn("genie: env file load failed", "path", path, "err", err)
			continue
		}
		slog.Info("genie: loaded env file", "path", path)
		break
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		if k := os.Getenv("SLEUTH_CLAUDE_API_KEY"); k != "" {
			_ = os.Setenv("ANTHROPIC_API_KEY", k)
		}
	}
}

// runServe boots the resolution pipeline and exposes it as MCP tools —
// run_query and list_providers — over stdio. Connect from Claude
// Desktop, Claude Code, mcp-inspector, or any MCP client.
//
// stdio is the only transport for v1. HTTP/SSE comes later.
//
// Hot-reload: a goroutine watches the config file and calls
// Genie.Reload on changes, so `genie mcp add` from another shell
// surfaces in this serve without a restart.
func runServe(ctx context.Context, _ []string) error {
	loadProjectEnv()

	g, err := genie.New(ctx, genie.Config{
		AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
	})
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()

	if g.ConfigPath() != "" {
		go func() {
			if err := g.WatchConfig(ctx); err != nil {
				slog.Warn("config watcher exited", "err", err)
			}
		}()
	}

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
