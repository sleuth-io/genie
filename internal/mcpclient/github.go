// Package mcpclient wraps an MCP server (currently just github-mcp-server)
// running as a stdio subprocess. The Client lists tools on connect and
// dispatches CallTool requests; package bridge.go adapts the tool surface
// into monty host functions.
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

// GitHubBinary is the executable name we spawn. Hardcoded per spike plan.
// Users install it however they like (`go install`, brew, docker via shim).
const GitHubBinary = "github-mcp-server"

// PATEnvVar is the env var the github-mcp-server expects.
const PATEnvVar = "GITHUB_PERSONAL_ACCESS_TOKEN"

// Client is a thin wrapper over mark3labs/mcp-go's stdio client. It owns the
// subprocess; Close tears it down.
type Client struct {
	mcp   *mcpc.Client
	tools []mcp.Tool
}

// OpenGitHub spawns `github-mcp-server stdio`, performs the MCP handshake,
// and lists tools. The returned Client is ready for Call.
//
// Errors fall into three buckets, distinguished only in the message:
//   - PAT missing — caller should set GITHUB_PERSONAL_ACCESS_TOKEN.
//   - binary missing — caller should install github-mcp-server.
//   - protocol/auth failure — surfaces upstream error verbatim.
func OpenGitHub(ctx context.Context) (*Client, error) {
	pat := os.Getenv(PATEnvVar)
	if pat == "" {
		return nil, fmt.Errorf("%s not set; export it before running (see .env.example)", PATEnvVar)
	}

	mc, err := mcpc.NewStdioMCPClient(GitHubBinary,
		[]string{PATEnvVar + "=" + pat},
		"stdio",
	)
	if err != nil {
		return nil, fmt.Errorf("spawn %s stdio: %w "+
			"(install with `go install github.com/github/github-mcp-server@latest` or brew)",
			GitHubBinary, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "intent-gw",
		Version: "spike",
	}
	if _, err := mc.Initialize(ctx, initReq); err != nil {
		_ = mc.Close()
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}

	listed, err := mc.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = mc.Close()
		return nil, fmt.Errorf("mcp list tools: %w", err)
	}

	return &Client{mcp: mc, tools: listed.Tools}, nil
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
