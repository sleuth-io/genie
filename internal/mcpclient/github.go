// Package mcpclient wraps an upstream MCP server running as a stdio
// subprocess. The Client lists tools on connect and dispatches CallTool
// requests; package bridge.go adapts the tool surface into monty host
// functions.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	mcpc "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// GitHubBinary is the executable name spawned by OpenGitHub. Users install
// it however they like (`go install`, brew, docker via shim).
const GitHubBinary = "github-mcp-server"

// PATEnvVar is the env var the github-mcp-server expects.
const PATEnvVar = "GITHUB_PERSONAL_ACCESS_TOKEN"

// ProviderSpec describes how to spawn an upstream MCP server.
//
// Env values are set on the child process only; they do not pollute the
// Genie process's environment. Two providers' env vars cannot clash and
// tokens do not leak between subprocesses.
type ProviderSpec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// Client is a thin wrapper over mark3labs/mcp-go's stdio client. It owns
// the subprocess; Close tears it down.
type Client struct {
	mcp   *mcpc.Client
	tools []mcp.Tool
}

// Open spawns the configured MCP server, performs the MCP handshake, and
// lists tools. The returned Client is ready for Call.
func Open(ctx context.Context, spec ProviderSpec) (*Client, error) {
	if spec.Command == "" {
		return nil, fmt.Errorf("provider %q: command is empty", spec.Name)
	}

	envPairs := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		envPairs = append(envPairs, k+"="+v)
	}

	mc, err := mcpc.NewStdioMCPClient(spec.Command, envPairs, spec.Args...)
	if err != nil {
		return nil, fmt.Errorf("provider %q: spawn %s: %w", spec.Name, spec.Command, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "genie",
		Version: "dev",
	}
	if _, err := mc.Initialize(ctx, initReq); err != nil {
		_ = mc.Close()
		return nil, fmt.Errorf("provider %q: mcp initialize: %w", spec.Name, err)
	}

	listed, err := mc.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = mc.Close()
		return nil, fmt.Errorf("provider %q: mcp list tools: %w", spec.Name, err)
	}

	return &Client{mcp: mc, tools: listed.Tools}, nil
}

// OpenGitHub is a convenience wrapper that builds the default GitHub MCP
// server ProviderSpec from the PAT in the environment.
func OpenGitHub(ctx context.Context) (*Client, error) {
	pat := os.Getenv(PATEnvVar)
	if pat == "" {
		return nil, fmt.Errorf("%s not set; export it before running (see .env.example)", PATEnvVar)
	}
	return Open(ctx, ProviderSpec{
		Name:    "github",
		Command: GitHubBinary,
		Args:    []string{"stdio"},
		Env:     map[string]string{PATEnvVar: pat},
	})
}

// Close shuts the subprocess down.
func (c *Client) Close() error {
	if c == nil || c.mcp == nil {
		return nil
	}
	return c.mcp.Close()
}

// Tools returns the discovered tool catalog. Caller MUST NOT mutate.
func (c *Client) Tools() []mcp.Tool {
	return c.tools
}

// Call invokes a tool by name with a string-keyed argument map.
//
// Return shape: if the server populated StructuredContent (preferred), that
// is returned as-is (typically map[string]any). Otherwise the first
// TextContent block is JSON-parsed; on parse failure, the raw text is
// returned as a string. IsError responses surface as Go errors so monty
// scripts can `try/except` cleanly.
func (c *Client) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	res, err := c.mcp.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", name, err)
	}
	if res.IsError {
		return nil, fmt.Errorf("tool %s returned error: %s", name, joinTextContent(res.Content))
	}
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	text := joinTextContent(res.Content)
	if text == "" {
		return nil, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err == nil {
		return parsed, nil
	}
	return text, nil
}

// joinTextContent concatenates the .text fields of TextContent items in
// order. Returns "" if no text content present.
func joinTextContent(items []mcp.Content) string {
	var out string
	for _, c := range items {
		if tc, ok := c.(mcp.TextContent); ok {
			out += tc.Text
		}
	}
	return out
}

// ErrNoTools is returned from Tools() callers when the server advertised
// none — usually a sign of misconfiguration (e.g. PAT scopes).
var ErrNoTools = errors.New("mcp server returned empty tool list")
