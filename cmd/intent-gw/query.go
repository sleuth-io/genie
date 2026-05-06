package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/mrdon/gqlspike/internal/engine"
)

// runQuery parses a GraphQL string from argv, walks each top-level node,
// and dispatches to a monty script. The resolver is backed by ./crystallized/;
// cache misses fall through to the LLM-backed plan.Generator, which generates
// a script, persists it, and returns it for immediate execution.
func runQuery(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New(`usage: intent-gw query "<graphql>"`)
	}
	queryStr := args[0]

	parsed, err := engine.Parse(queryStr)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	bundle, err := setupEngine(ctx, "./crystallized")
	if err != nil {
		return err
	}
	defer bundle.Close()

	out, err := bundle.executor.Execute(ctx, parsed)
	if err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	// TODO(drift): re-resolve 1-in-N + diff against cached replay; evict on
	// drift > threshold. Cut from the spike (FR-6) — see plan file.

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return nil
}
