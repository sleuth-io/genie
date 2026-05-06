package main

import (
	"context"
	"fmt"

	"github.com/mrdon/gqlspike/internal/mcpclient"
)

// runMCPTools connects to github-mcp-server stdio and prints the tool catalog.
// Validates: PAT is set, binary spawns, MCP handshake completes, tools list.
func runMCPTools(ctx context.Context, _ []string) error {
	c, err := mcpclient.OpenGitHub(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	tools := c.Tools()
	fmt.Printf("github-mcp-server advertises %d tools:\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  %-40s  %s\n", t.Name, oneLine(t.Description))
	}
	return nil
}

// oneLine collapses a multi-line description to its first non-empty line.
func oneLine(s string) string {
	for _, line := range splitLines(s) {
		if line != "" {
			return line
		}
	}
	return ""
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
