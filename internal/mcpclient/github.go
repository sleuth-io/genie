// Package mcpclient wraps an upstream MCP server. The Client lists
// tools on connect and dispatches CallTool requests; package
// bridge.go adapts the tool surface into monty host functions.
//
// Two transport shapes are supported, selected by ProviderSpec:
//
//   - stdio: spawn a local subprocess and pipe MCP over its stdio.
//   - http/sse: connect to a remote URL, optionally with OAuth.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	mcpc "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// GitHubBinary is the executable name spawned by OpenGitHub. Users install
// it however they like (`go install`, brew, docker via shim).
const GitHubBinary = "github-mcp-server"

// PATEnvVar is the env var the github-mcp-server expects.
const PATEnvVar = "GITHUB_PERSONAL_ACCESS_TOKEN"

// ProviderSpec describes how to reach one upstream MCP server. Either
// Command (stdio transport) or URL (http/sse transport) is set, never
// both — config validation enforces this.
//
// Env values are set on the child process only (stdio); they do not
// pollute the Genie process's environment. Headers (HTTP transport)
// are sent verbatim with each request.
type ProviderSpec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Type    string // "stdio", "http", "sse" (defaults to stdio if Command set, http if URL set)
	Scopes  []string
	Headers map[string]string

	// OAuthTokenStore, when non-nil, enables OAuth on the HTTP/SSE
	// transports. The handler stores access + refresh tokens here
	// and attaches Authorization headers automatically. Initialize
	// returns an OAuthAuthorizationRequiredError when the store is
	// empty (or refresh failed) — the caller runs the interactive
	// flow and retries Open.
	OAuthTokenStore transport.TokenStore

	// OAuthClientID + OAuthClientSecret are reused from a previous
	// dynamic client registration. Empty on the very first connect;
	// the caller's auth flow populates them and re-Opens.
	OAuthClientID     string
	OAuthClientSecret string
	OAuthRedirectURI  string
}

// Client is a thin wrapper over mark3labs/mcp-go. It owns the
// underlying transport (subprocess or HTTP connection); Close tears
// it down.
type Client struct {
	mcp   *mcpc.Client
	tools []mcp.Tool
}

// OAuthRequiredError is returned from Open when the HTTP transport
// surfaces an OAuthAuthorizationRequiredError. The caller is
// expected to run an interactive auth flow that populates the
// associated TokenStore, then retry Open.
type OAuthRequiredError struct {
	Provider string
	URL      string
	Cause    error
}

func (e *OAuthRequiredError) Error() string {
	return fmt.Sprintf("provider %q at %s requires authorization", e.Provider, e.URL)
}

func (e *OAuthRequiredError) Unwrap() error { return e.Cause }

// Open dispatches on transport type, performs the MCP handshake, and
// lists tools. The returned Client is ready for Call.
func Open(ctx context.Context, spec ProviderSpec) (*Client, error) {
	if spec.URL != "" {
		return openHTTP(ctx, spec)
	}
	return openStdio(ctx, spec)
}

func openStdio(ctx context.Context, spec ProviderSpec) (*Client, error) {
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

	tools, err := initAndList(ctx, mc, spec.Name)
	if err != nil {
		_ = mc.Close()
		return nil, err
	}

	return &Client{mcp: mc, tools: tools}, nil
}

func openHTTP(ctx context.Context, spec ProviderSpec) (*Client, error) {
	useSSE := spec.Type == "sse"
	useOAuth := spec.OAuthTokenStore != nil

	var (
		mc  *mcpc.Client
		err error
	)
	switch {
	case useOAuth && useSSE:
		oauthCfg := transport.OAuthConfig{
			ClientID:     spec.OAuthClientID,
			ClientSecret: spec.OAuthClientSecret,
			RedirectURI:  spec.OAuthRedirectURI,
			Scopes:       spec.Scopes,
			TokenStore:   spec.OAuthTokenStore,
			PKCEEnabled:  true,
		}
		mc, err = mcpc.NewOAuthSSEClient(spec.URL, oauthCfg, transport.WithHeaders(spec.Headers))
	case useOAuth:
		oauthCfg := transport.OAuthConfig{
			ClientID:     spec.OAuthClientID,
			ClientSecret: spec.OAuthClientSecret,
			RedirectURI:  spec.OAuthRedirectURI,
			Scopes:       spec.Scopes,
			TokenStore:   spec.OAuthTokenStore,
			PKCEEnabled:  true,
		}
		mc, err = mcpc.NewOAuthStreamableHttpClient(spec.URL, oauthCfg, transport.WithHTTPHeaders(spec.Headers))
	case useSSE:
		mc, err = mcpc.NewSSEMCPClient(spec.URL, mcpc.WithHeaders(spec.Headers), mcpc.WithHTTPClient(http.DefaultClient))
	default:
		mc, err = mcpc.NewStreamableHttpClient(spec.URL, transport.WithHTTPHeaders(spec.Headers))
	}
	if err != nil {
		return nil, fmt.Errorf("provider %q: build %s client: %w", spec.Name, spec.Type, err)
	}

	if err := mc.Start(ctx); err != nil {
		_ = mc.Close()
		if isOAuthRequired(err) {
			return nil, &OAuthRequiredError{Provider: spec.Name, URL: spec.URL, Cause: err}
		}
		return nil, fmt.Errorf("provider %q: start: %w", spec.Name, err)
	}

	tools, err := initAndList(ctx, mc, spec.Name)
	if err != nil {
		_ = mc.Close()
		if isOAuthRequired(err) {
			return nil, &OAuthRequiredError{Provider: spec.Name, URL: spec.URL, Cause: err}
		}
		return nil, err
	}

	return &Client{mcp: mc, tools: tools}, nil
}

func isOAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	if mcpc.IsOAuthAuthorizationRequiredError(err) {
		return true
	}
	return errors.Is(err, transport.ErrOAuthAuthorizationRequired)
}

func initAndList(ctx context.Context, mc *mcpc.Client, name string) ([]mcp.Tool, error) {
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "genie",
		Version: "dev",
	}
	if _, err := mc.Initialize(ctx, initReq); err != nil {
		return nil, fmt.Errorf("provider %q: mcp initialize: %w", name, err)
	}
	listed, err := mc.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("provider %q: mcp list tools: %w", name, err)
	}
	return listed.Tools, nil
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
