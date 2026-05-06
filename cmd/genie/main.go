// Command genie is the CLI + MCP-server entry point for Genie, a smart
// MCP client that fronts upstream MCP servers and shapes responses.
//
//	genie query "<graphql>"   one-shot CLI: parse, resolve, print JSON.
//	genie serve               start MCP server exposing run_query.
//	genie eval                run the curated eval set, print metrics.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/sleuth-io/genie/internal/buildinfo"
	"github.com/sleuth-io/genie/internal/envfile"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := envfile.Load(); err != nil {
		slog.Error("failed loading env file", "err", err)
		os.Exit(1)
	}
	canonicalizeEnv()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "query":
		err = runQuery(ctx, args)
	case "serve":
		err = runServe(ctx, args)
	case "eval":
		err = runEval(ctx, args)
	case "-h", "--help", "help":
		usage()
		return
	case "-v", "--version", "version":
		fmt.Printf("genie %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		slog.Error("command failed", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

// canonicalizeEnv copies values from project-specific aliases into the
// canonical env vars the rest of the codebase reads. Lets a teammate point
// Genie at an existing Sleuth .env without renaming entries; if the
// canonical name is already set (manual export), the alias is ignored.
func canonicalizeEnv() {
	aliases := map[string]string{
		"GITHUB_PERSONAL_ACCESS_TOKEN": "SLEUTH_TEST_GITHUB_TOKEN",
		"ANTHROPIC_API_KEY":            "SLEUTH_CLAUDE_API_KEY",
	}
	for canonical, alias := range aliases {
		if _, set := os.LookupEnv(canonical); set {
			continue
		}
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			_ = os.Setenv(canonical, v)
		}
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `genie — smart MCP client for agents

Usage:
  genie query "<graphql>"   resolve one query, print JSON
  genie serve               start MCP server (run_query, list_providers)
  genie eval                run curated eval set

Required env (set directly or via .env):
  GITHUB_PERSONAL_ACCESS_TOKEN  GitHub PAT, forwarded to github-mcp-server
  ANTHROPIC_API_KEY             Anthropic key for plan generation

Optional:
  GENIE_ENV_FILE                override path to env file (default: ./.env)
  GENIE_CONFIG                  override path to config file
  GENIE_CACHE_DIR               override cache directory
`)
}
