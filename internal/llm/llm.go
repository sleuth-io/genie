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

// Client is the LLM call surface plan.Generator depends on for
// single-turn calls (NORMALIZE, plain GENERATE).
type Client interface {
	Generate(ctx context.Context, system []SystemBlock, userText string) (Response, error)
}

// ChatClient is the optional multi-turn surface used by GENERATE's
// tool-use loop. Backends that can drive tool-use rounds (the
// Anthropic SDK) implement it; backends that can't (the claude CLI
// in non-interactive mode) don't, and plan.Generator falls back to
// single-turn Generate.
//
// Type-assert to detect support: `cc, ok := client.(ChatClient)`.
type ChatClient interface {
	Client
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// ToolDef declares a tool the model may call during a Chat turn.
// The InputSchema is the JSON-Schema description of expected args.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolUse is one tool call the model emitted in a turn. ID is the
// model-assigned identifier; the caller pairs it with a ToolResult.
type ToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is the caller's response to a ToolUse, fed back as a
// user message on the next Chat turn.
type ToolResult struct {
	ToolUseID string
	Content   string // serialized tool result, typically JSON
	IsError   bool
}

// Message is one entry in the multi-turn conversation. Role is
// "user" or "assistant". A user message carries either Text (plain
// prompt) or ToolResults (responses to a prior assistant ToolUse
// turn). An assistant message carries Text and/or ToolUses.
type Message struct {
	Role        string
	Text        string
	ToolUses    []ToolUse    // assistant only
	ToolResults []ToolResult // user only
}

// ChatRequest is one multi-turn LLM invocation. The caller passes
// the running message history (each Chat returns the new assistant
// turn; the caller appends tool results and re-invokes for the next
// round). Tools is the set of catalog tools exposed for this loop.
type ChatRequest struct {
	System   []SystemBlock
	Messages []Message
	Tools    []ToolDef
	// ToolChoice constrains the model's choice. Empty/auto = model
	// decides; "any" = must call some tool; {"type":"tool","name":"X"}
	// would force a specific tool but isn't currently used.
	ToolChoice string
}

// ChatResponse is one assistant turn. StopReason is "end_turn" when
// the model produced text without calling a tool, "tool_use" when it
// emitted ToolUses (caller should respond with ToolResults and call
// Chat again), or "max_tokens" / other on truncation.
type ChatResponse struct {
	Text       string
	ToolUses   []ToolUse
	Usage      Usage
	StopReason string
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

// effortKey is the context value key for per-call thinking-effort
// overrides. plan.Generator sets EffortDisabled for NORMALIZE (small
// structured output, no reasoning needed) and leaves it default for
// GENERATE. Backends translate to their native equivalent (SDK:
// Thinking config; CLI: --effort flag).
type effortKey struct{}

// Effort levels. Empty string ⇒ backend default.
const (
	EffortDisabled = "disabled" // SDK: OfDisabled. CLI: --effort low.
)

// WithEffort attaches a thinking-effort hint to ctx.
func WithEffort(ctx context.Context, level string) context.Context {
	if level == "" {
		return ctx
	}
	return context.WithValue(ctx, effortKey{}, level)
}

// EffortFromContext returns the effort level attached to ctx, or "".
func EffortFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(effortKey{}).(string); ok {
		return v
	}
	return ""
}

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
