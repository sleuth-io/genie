// Command genie is the CLI + MCP-server entry point for Genie, a smart
// MCP client that fronts upstream MCP servers and shapes responses.
//
//	genie mcp add ...           register a provider
//	genie auth ...              run/manage the OAuth flow
//	genie serve                 start MCP server exposing run_query.
//	genie query "<graphql>"     one-shot CLI: parse, resolve, print JSON.
//
// `genie eval` is also wired in for build/CI (see Makefile target
// `make eval`) but isn't documented for end users.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sleuth-io/genie/internal/buildinfo"
	"github.com/sleuth-io/genie/internal/logger"
)

func main() {
	logger.SetDefault()

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
	case "auth":
		err = runAuth(ctx, args)
	case "mcp":
		err = runMCP(ctx, args)
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
		logger.Get().Error("command failed", "cmd", cmd, "err", err)
		fmt.Fprintf(os.Stderr, "genie: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `genie — smart MCP client for agents

Usage:
  genie mcp add -n NAME -u URL                add an http/sse provider (auto-auth)
  genie mcp add -n NAME -c "CMD [ARGS...]"    add a stdio provider
  genie mcp list                              list configured providers
  genie mcp remove NAME                       drop a provider
  genie mcp import                            import MCP servers from Claude Code
  genie auth <provider>     re-run the OAuth flow for an http/sse provider
  genie auth list           show auth status for each provider
  genie query "<graphql>"   resolve one query, print JSON
  genie serve               start MCP server (run_query, list_providers)

Env:
  ANTHROPIC_API_KEY    plan-generation key (or use the claude CLI fallback)
  GENIE_LLM_BACKEND    pin LLM backend: anthropic-sdk|claude-cli
  GENIE_AUTH_BACKEND   pin token storage: keyring|file
  GENIE_CONFIG         override config path (default: $XDG_CONFIG_HOME/genie/config.json)
  GENIE_CACHE_DIR      override cache dir (default: $XDG_CACHE_HOME/genie/crystallized)
`)
}
