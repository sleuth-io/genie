// Package llm abstracts the LLM call surface plan.Generator depends on
// so Genie can swap between backends:
//
//   - Anthropic SDK (direct API call). Requires ANTHROPIC_API_KEY.
//   - Claude Code CLI subprocess. Used when no key is set but the
//     `claude` binary is on PATH — typical when Genie is running
//     under Claude Code.
//
// New backends (OpenAI, Codex CLI, Bedrock, etc.) slot in by
// implementing Client and adding a branch to Select. plan.Generator
// only knows about the Client interface, so adding a backend requires
// no changes outside this package.
package llm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// SystemBlock is one chunk of the system prompt. CacheBreakAfter is a
// hint to the SDK backend that a prompt-cache breakpoint should land
// at the end of this block. The CLI backend ignores it (Claude Code
// manages its own cache).
type SystemBlock struct {
	Text            string
	CacheBreakAfter bool
}

// Usage tracks tokens for one Generate call. The CLI backend reports
// only what Claude Code surfaces in its stream-json result event;
// fields it doesn't surface stay zero.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// Response is what Generate returns: the assembled text plus best-
// effort usage stats.
type Response struct {
	Text  string
	Usage Usage
}

// Client is the LLM call surface plan.Generator depends on.
type Client interface {
	Generate(ctx context.Context, system []SystemBlock, userText string) (Response, error)
}

// Backend names returned by Select for logging.
const (
	BackendAnthropicSDK = "anthropic-sdk"
	BackendClaudeCLI    = "claude-cli"
)

// BackendEnvVar lets a user pin a backend explicitly, bypassing
// auto-detection. Accepted values match the Backend* constants.
const BackendEnvVar = "GENIE_LLM_BACKEND"

// modelKey is the context value key for per-call model overrides.
// The plan generator sets a model per call-type (normalize vs
// generate); each backend reads it back here and threads it into
// its API/CLI invocation. Empty string ⇒ use the backend's default.
type modelKey struct{}

// WithModel attaches a model identifier to ctx. Empty s leaves ctx
// unchanged so callers can branch-free pass through.
func WithModel(ctx context.Context, s string) context.Context {
	if s == "" {
		return ctx
	}
	return context.WithValue(ctx, modelKey{}, s)
}

// ModelFromContext returns the model attached to ctx, or "".
func ModelFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(modelKey{}).(string); ok {
		return v
	}
	return ""
}

// Select picks a backend based on environment and explicit config.
// Auto-detection precedence (no GENIE_LLM_BACKEND set):
//
//  1. apiKey != "" → Anthropic SDK with that key.
//  2. ANTHROPIC_API_KEY in env → Anthropic SDK.
//  3. `claude` binary on PATH → Claude Code CLI subprocess.
//  4. error.
//
// New backends slot in between (3) and the error: e.g. OPENAI_API_KEY
// → OpenAI, `codex` on PATH → Codex CLI.
//
// Returns the chosen Client and a short label naming the backend (for
// logging).
func Select(apiKey string) (Client, string, error) {
	if forced := os.Getenv(BackendEnvVar); forced != "" {
		return selectForced(forced, apiKey)
	}
	if apiKey != "" {
		return NewAnthropic(apiKey), BackendAnthropicSDK, nil
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return NewAnthropic(""), BackendAnthropicSDK, nil
	}
	if _, err := exec.LookPath("claude"); err == nil {
		slog.Info("llm: ANTHROPIC_API_KEY not set; falling back to claude CLI")
		return NewClaudeCLI(), BackendClaudeCLI, nil
	}
	return nil, "", fmt.Errorf("no LLM backend available: set ANTHROPIC_API_KEY or install the Claude Code CLI (`claude` on PATH)")
}

func selectForced(name, apiKey string) (Client, string, error) {
	switch name {
	case BackendAnthropicSDK:
		return NewAnthropic(apiKey), BackendAnthropicSDK, nil
	case BackendClaudeCLI:
		if _, err := exec.LookPath("claude"); err != nil {
			return nil, "", fmt.Errorf("%s=%s but `claude` binary not on PATH: %w", BackendEnvVar, name, err)
		}
		return NewClaudeCLI(), BackendClaudeCLI, nil
	default:
		return nil, "", fmt.Errorf("%s=%s not recognised (known: %s, %s)",
			BackendEnvVar, name, BackendAnthropicSDK, BackendClaudeCLI)
	}
}
