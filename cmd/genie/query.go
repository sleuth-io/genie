package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/sleuth-io/genie/pkg/genie"
)

// runQuery resolves a single GraphQL-shaped query against one of the
// configured providers, prints the JSON result, and exits.
func runQuery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	provider := fs.String("provider", "github", "provider name (must match an entry in your config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return errors.New(`usage: genie query [--provider NAME] "<graphql>"`)
	}
	queryStr := rest[0]

	g, err := genie.New(ctx, genie.Config{
		AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
	})
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()

	out, err := g.QueryMap(ctx, *provider, queryStr)
	if err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return nil
}
