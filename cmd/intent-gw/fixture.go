package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mrdon/gqlspike/internal/mcpclient"
	"github.com/mrdon/gqlspike/internal/runtime"
)

// runFixture executes a hand-written monty script against the live GitHub
// MCP server. It's the Day-2 deliverable: proof that a python script can
// invoke MCP tools through the host bridge and shape JSON output.
//
// Args (positional, optional):
//
//	owner = $1 or "anthropics"
//	repo  = $2 or "anthropic-sdk-python"
//
// The script asks for the most recent open PRs and projects each down to
// {title, number, author}.
const fixtureScript = `
def execute(owner, repo):
    raw = github_list_pull_requests(owner=owner, repo=repo, state="open", perPage=5)
    items = raw.get("items", raw) if isinstance(raw, dict) else raw
    out = []
    for pr in items:
        out.append({
            "title": pr.get("title"),
            "number": pr.get("number"),
            "author": (pr.get("user") or {}).get("login"),
        })
    return out
`

func runFixture(ctx context.Context, args []string) error {
	owner := "anthropics"
	repo := "anthropic-sdk-python"
	if len(args) >= 1 {
		owner = args[0]
	}
	if len(args) >= 2 {
		repo = args[1]
	}

	mc, err := mcpclient.OpenGitHub(ctx)
	if err != nil {
		return err
	}
	defer mc.Close()

	eng, err := runtime.NewMontyEngineOwned()
	if err != nil {
		return fmt.Errorf("init monty engine: %w", err)
	}
	defer eng.Close()

	mod, err := eng.Compile(fixtureScript)
	if err != nil {
		return fmt.Errorf("compile fixture: %w", err)
	}

	builtIns, params := mcpclient.BuildHostFunctions(mc)
	caps := &runtime.Capabilities{
		BuiltIns:      builtIns,
		BuiltInParams: params,
		Limits: runtime.Limits{
			MaxDuration: 30 * time.Second,
		},
	}

	result, meta, err := eng.Run(ctx, mod, "execute",
		map[string]any{"owner": owner, "repo": repo}, caps)
	if err != nil {
		return fmt.Errorf("run fixture: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	fmt.Fprintf(os.Stderr, "fixture: duration_ms=%d external_calls=%d\n",
		meta.DurationMs, meta.ExternalCalls)
	return nil
}
