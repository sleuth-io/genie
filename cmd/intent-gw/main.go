// Command intent-gw is the CLI + MCP-server entry point for the intent-gateway
// spike. Subcommands:
//
//	intent-gw query "<graphql>"   one-shot CLI: parse, resolve, print JSON.
//	intent-gw serve               start MCP server exposing run_query.
//	intent-gw eval                run the curated eval set, print metrics.
//	intent-gw smoke               smoke-test the embedded monty runtime.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/mrdon/gqlspike/internal/envfile"
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
	case "smoke":
		err = runSmoke(ctx, args)
	case "mcp-tools":
		err = runMCPTools(ctx, args)
	case "fixture":
		err = runFixture(ctx, args)
	case "probe":
		err = runProbe(ctx, args)
	case "-h", "--help", "help":
		usage()
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
// the spike at an existing Sleuth .env without renaming entries; if the
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
	fmt.Fprintf(os.Stderr, `intent-gw — intent-gateway spike

Usage:
  intent-gw query "<graphql>"   resolve one query, print JSON
  intent-gw serve               start MCP server (run_query tool)
  intent-gw eval                run curated eval set
  intent-gw smoke               smoke-test embedded monty runtime
  intent-gw mcp-tools           list tools advertised by github-mcp-server
  intent-gw fixture             run a hand-written monty script vs. GitHub

Required env (set directly or via .env in CWD):
  GITHUB_PERSONAL_ACCESS_TOKEN  GitHub PAT, forwarded to github-mcp-server
  ANTHROPIC_API_KEY             Anthropic key for plan generation

Optional:
  INTENT_GW_ENV_FILE            override path to env file (default: ./.env)
`)
}
