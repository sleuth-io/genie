package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
)

type claudeCLI struct {
	bin   string
	model string
}

// NewClaudeCLI builds a Client that shells out to the `claude` CLI
// (Claude Code). It does not need an API key — Claude Code manages
// its own auth. Plan generation prompts are passed as a single -p
// arg with the system prompt prefixed and tool use disabled.
func NewClaudeCLI() Client {
	return &claudeCLI{bin: "claude"}
}

func (c *claudeCLI) Generate(ctx context.Context, system []SystemBlock, userText string) (Response, error) {
	var prompt strings.Builder
	for _, b := range system {
		prompt.WriteString(b.Text)
		prompt.WriteString("\n\n")
	}
	prompt.WriteString("---\n\n")
	prompt.WriteString(userText)

	// Genie's plan-generation prompts don't need any of Claude
	// Code's interactive scaffolding — no MCP servers (we ARE the
	// MCP server here), no skills, no per-machine context
	// injection, no session persistence. Each spawn pays setup
	// cost for everything we don't disable, easily 5–10s/call. The
	// flags below are individually safe under OAuth auth (unlike
	// --bare, which requires ANTHROPIC_API_KEY).
	args := []string{
		"-p", prompt.String(),
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", "",
		"--strict-mcp-config",                      // skip all MCP servers (none passed via --mcp-config)
		"--disable-slash-commands",                 // skip skill discovery
		"--exclude-dynamic-system-prompt-sections", // skip cwd/git/env injection
		"--no-session-persistence",                 // skip writing transcript to ~/.claude
	}
	model := ModelFromContext(ctx)
	if model == "" {
		model = c.model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if EffortFromContext(ctx) == EffortDisabled {
		args = append(args, "--effort", "low")
	}

	cmd := exec.CommandContext(ctx, c.bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Response{}, fmt.Errorf("claude cli: stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Response{}, fmt.Errorf("claude cli: start: %w", err)
	}

	var (
		text      strings.Builder
		usage     Usage
		gotResult bool
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		var eventType string
		if raw, ok := event["type"]; ok {
			_ = json.Unmarshal(raw, &eventType)
		}
		switch eventType {
		case "assistant":
			// Streaming text deltas accumulate in the result event;
			// we collect them here too in case result is missing.
			var msg struct {
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					Usage struct {
						InputTokens         int64 `json:"input_tokens"`
						OutputTokens        int64 `json:"output_tokens"`
						CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
						CacheReadTokens     int64 `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if !gotResult {
				text.Reset()
				for _, b := range msg.Message.Content {
					if b.Type == "text" {
						text.WriteString(b.Text)
					}
				}
			}
			if msg.Message.Usage.InputTokens > 0 {
				usage.InputTokens = msg.Message.Usage.InputTokens
				usage.OutputTokens = msg.Message.Usage.OutputTokens
				usage.CacheCreationTokens = msg.Message.Usage.CacheCreationTokens
				usage.CacheReadTokens = msg.Message.Usage.CacheReadTokens
			}
		case "result":
			var result struct {
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			_ = json.Unmarshal([]byte(line), &result)
			if result.IsError {
				_ = cmd.Wait()
				return Response{}, fmt.Errorf("claude cli: %s", result.Result)
			}
			if result.Result != "" {
				gotResult = true
				text.Reset()
				text.WriteString(result.Result)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return Response{Text: text.String(), Usage: usage}, ctx.Err()
		}
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}
		return Response{}, fmt.Errorf("claude cli failed: %s", errMsg)
	}

	if text.Len() == 0 {
		return Response{}, fmt.Errorf("claude cli: empty response (stderr: %s)", stderr.String())
	}

	return Response{Text: text.String(), Usage: usage}, nil
}

// Drive runs the explore-then-submit loop through the claude CLI in
// stream-json input/output mode. The CLI runs the model's tool-use
// loop INTERNALLY (claude's own MCP-using machinery) — Genie just
// observes the resulting events to capture fixtures and pulls the
// final submit content out of the assistant's last message.
//
// Two structural differences from the SDK driver:
//
//  1. Tool definitions can't be injected. Tools available to the
//     model are whatever claude code already has loaded from the
//     user's MCP config. We pass --allowedTools "mcp__<provider>__*"
//     to scope to the upstream provider's tool set; users who
//     haven't configured the same provider in claude code separately
//     from Genie will see an empty tool list and the loop will be
//     useless. (A future improvement: spawn a `genie tool-proxy
//     --provider=X` subcommand and point claude at it via
//     --mcp-config, so Genie's own auth/connection is reused.)
//
//  2. The synthetic submit_script tool isn't a real tool here. The
//     prompt instructs the model to terminate by emitting JSON of
//     shape {"<SubmitToolName>": <input>} as its final assistant
//     message; we parse it from text. A bit more fragile than a
//     structured tool_use block but fine for the spike.
//
// Stateless per call: each Drive spawns a new claude process. For
// revision turns plan.Generator passes the extended conversation
// in DriveRequest.Messages.
func (c *claudeCLI) Drive(ctx context.Context, req DriveRequest) (LoopResult, error) {
	if len(req.Messages) == 0 {
		return LoopResult{}, fmt.Errorf("claude cli drive: no messages")
	}

	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--exclude-dynamic-system-prompt-sections",
		"--disable-slash-commands",
	}

	// Restrict the model to upstream-provider tools. claude's
	// --allowedTools doesn't support `mcp__<provider>__*` wildcards
	// (the syntax is positive list of exact names), so we
	// enumerate the tools we know about from req.Tools, prefixing
	// each with `mcp__<provider>__`. The synthetic SubmitToolName
	// has no claude-side counterpart and is omitted.
	if req.Provider != "" && len(req.Tools) > 0 {
		names := make([]string, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Name == req.SubmitToolName {
				continue
			}
			// Strip Genie's monty-side prefix (e.g. github_) before
			// applying claude's mcp__<provider>__ prefix.
			bare := t.Name
			if i := strings.Index(bare, "_"); i >= 0 && bare[:i+1] == "github_" {
				bare = bare[i+1:]
			}
			names = append(names, "mcp__"+req.Provider+"__"+bare)
		}
		if len(names) > 0 {
			args = append(args, "--allowedTools", strings.Join(names, ","))
		}
	}

	// Build the system prompt: existing blocks + a CLI-specific coda.
	var sysBuilder strings.Builder
	for _, b := range req.System {
		sysBuilder.WriteString(b.Text)
		sysBuilder.WriteString("\n\n")
	}
	if req.SubmitToolName != "" && req.Provider != "" {
		fmt.Fprintf(&sysBuilder, `

## Backend-specific notes (claude CLI)

### Tool names — DIFFERENT in chat vs in your script

In THIS conversation you call tools as `+"`mcp__%s__<name>`"+` — that is
how claude's MCP machinery exposes them. Use that exact prefix when
calling tools in your turns.

In the SCRIPT you submit, call tools as `+"`github_<name>`"+` — that is
the host-function prefix the monty sandbox registers. The provider
component is dropped in script-side names; the `+"`github_`"+` prefix is
hardcoded regardless of which provider you're using.

So: same upstream tool, two names. During exploration here you'd call
`+"`mcp__%s__getX`"+`. Inside your monty_script, you write
`+"`github_getX(...)`"+`.

### Submit by emitting JSON, not by calling a tool

The synthetic %q tool is NOT available as a real tool here. When
you have finished exploring and would otherwise call it, terminate
by writing your FINAL assistant message as ONLY a JSON object:

    {%q: {<the input you'd pass>}}

No prose around it. The first character of your final message must
be `+"`{`"+` and the last must be `+"`}`"+`. Genie parses that
message as the submission and runs verification.
`, req.Provider, req.Provider, req.SubmitToolName, req.SubmitToolName)
	}
	args = append(args, "--system-prompt", sysBuilder.String())

	if model := ModelFromContext(ctx); model != "" {
		args = append(args, "--model", model)
	}
	if EffortFromContext(ctx) == EffortDisabled {
		args = append(args, "--effort", "low")
	}

	cmd := exec.CommandContext(ctx, c.bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return LoopResult{}, fmt.Errorf("claude cli: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return LoopResult{}, fmt.Errorf("claude cli: stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return LoopResult{}, fmt.Errorf("claude cli: start: %w", err)
	}
	defer func() {
		_ = cmd.Wait()
	}()

	// Stream every llm.Message in req.Messages as a separate stream-json
	// user message. The CLI's response between messages is consumed
	// inline (via streamReader); the loop ends when the last message's
	// response arrives.
	streamReader := newCLIStreamReader(stdout, req.SubmitToolName)
	go streamReader.run()

	for i, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		// For revision turns the caller may stack multiple user
		// messages with intervening assistant messages — for the
		// CLI driver we only forward user messages and let the
		// model regenerate from there (we can't actually replay
		// assistant turns mid-stream).
		text := m.Text
		if text == "" && len(m.ToolResults) > 0 {
			// Render tool results as plain text — the CLI manages
			// real tool_result blocks itself, so any results carried
			// in the request are revision-time hints not loop state.
			var tb strings.Builder
			for _, tr := range m.ToolResults {
				tb.WriteString(tr.Content)
				tb.WriteString("\n")
			}
			text = tb.String()
		}
		if err := writeStreamJSONUserMessage(stdin, text); err != nil {
			_ = stdin.Close()
			return LoopResult{}, fmt.Errorf("claude cli: write user message %d: %w", i, err)
		}
	}
	_ = stdin.Close()

	res, runErr := streamReader.collect()
	if runErr != nil {
		errMsg := stderr.String()
		if errMsg != "" {
			return res, fmt.Errorf("%w (stderr: %s)", runErr, errMsg)
		}
		return res, runErr
	}
	return res, nil
}

// writeStreamJSONUserMessage encodes one user message in claude's
// stream-json input format and writes it (newline-terminated) to
// stdin.
func writeStreamJSONUserMessage(w io.Writer, text string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
		"session_id":         "default",
		"parent_tool_use_id": nil,
	}
	buf, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(buf, '\n')); err != nil {
		return err
	}
	return nil
}

// cliStreamReader reads stream-json events from claude's stdout and
// translates them into a LoopResult. Owns the scan loop in its own
// goroutine; collect() blocks until the stream closes (and the CLI
// has emitted its final result event) or the context is cancelled.
type cliStreamReader struct {
	r              io.Reader
	submitToolName string
	res            LoopResult
	finalText      strings.Builder
	pendingCalls   map[string]Observation
	done           chan struct{}
	err            error
}

func newCLIStreamReader(r io.Reader, submitToolName string) *cliStreamReader {
	return &cliStreamReader{
		r:              r,
		submitToolName: submitToolName,
		pendingCalls:   make(map[string]Observation),
		done:           make(chan struct{}),
	}
}

func (s *cliStreamReader) run() {
	defer close(s.done)
	scanner := bufio.NewScanner(s.r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		s.handleEvent(line)
	}
	if err := scanner.Err(); err != nil {
		s.err = fmt.Errorf("scan stdout: %w", err)
	}
}

func (s *cliStreamReader) collect() (LoopResult, error) {
	<-s.done
	if s.err != nil {
		return s.res, s.err
	}
	// Final-message parse: the model is asked to emit submit_script
	// JSON as its terminal assistant text. Parse leniently.
	if s.res.Submit == nil && s.finalText.Len() > 0 {
		if parsed := parseSubmitFromText(s.finalText.String(), s.submitToolName); parsed != nil {
			s.res.Submit = parsed
		} else {
			s.res.FinalText = s.finalText.String()
		}
	}
	return s.res, nil
}

func (s *cliStreamReader) handleEvent(line string) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return
	}
	var eventType string
	if raw, ok := event["type"]; ok {
		_ = json.Unmarshal(raw, &eventType)
	}
	switch eventType {
	case "assistant":
		s.handleAssistant(line)
	case "user":
		s.handleUser(line)
	case "result":
		s.handleResult(line)
	}
}

func (s *cliStreamReader) handleAssistant(line string) {
	var msg struct {
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
			Usage struct {
				InputTokens         int64 `json:"input_tokens"`
				OutputTokens        int64 `json:"output_tokens"`
				CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return
	}
	if msg.Message.Usage.InputTokens > 0 {
		s.res.Usage.InputTokens += msg.Message.Usage.InputTokens
		s.res.Usage.OutputTokens += msg.Message.Usage.OutputTokens
		s.res.Usage.CacheCreationTokens += msg.Message.Usage.CacheCreationTokens
		s.res.Usage.CacheReadTokens += msg.Message.Usage.CacheReadTokens
	}
	for _, b := range msg.Message.Content {
		switch b.Type {
		case "text":
			// Each assistant block is the latest snapshot of the
			// running text — overwrite rather than append.
			s.finalText.Reset()
			s.finalText.WriteString(b.Text)
		case "tool_use":
			s.pendingCalls[b.ID] = Observation{
				ToolName: b.Name,
				Args:     b.Input,
			}
		}
	}
}

func (s *cliStreamReader) handleUser(line string) {
	// User events here carry tool_result blocks (claude's MCP layer
	// dispatched the tool, this is the response). Pair by tool_use_id
	// against pendingCalls and emit an Observation.
	var msg struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   any    `json:"content"`
				IsError   bool   `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return
	}
	for _, b := range msg.Message.Content {
		if b.Type != "tool_result" {
			continue
		}
		obs, ok := s.pendingCalls[b.ToolUseID]
		if !ok {
			continue
		}
		delete(s.pendingCalls, b.ToolUseID)
		if b.IsError {
			continue
		}
		// Tool results are typically a list of {type:"text", text:"..."}
		// blocks — unwrap to a parsed JSON value where possible.
		obs.Result = unwrapToolResult(b.Content)
		s.res.Observations = append(s.res.Observations, obs)
	}
}

func (s *cliStreamReader) handleResult(line string) {
	var result struct {
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`
		NumTurns  int    `json:"num_turns"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(line), &result); err == nil {
		if result.IsError {
			s.err = fmt.Errorf("claude cli: %s", result.Result)
			return
		}
		// `result` event's `result` field is the model's final
		// assistant text. Use it as the authoritative final text
		// (overrides streaming snapshots).
		if result.Result != "" {
			s.finalText.Reset()
			s.finalText.WriteString(result.Result)
		}
	}
}

// unwrapToolResult turns claude's tool_result content (often a list
// of {type:"text", text:"<json>"} blocks) into the Go value the model
// observed. Best-effort: if the text decodes as JSON, return that;
// otherwise return the raw concatenated text. Non-text blocks (e.g.
// images) pass through as their decoded structure.
func unwrapToolResult(content any) any {
	arr, ok := content.([]any)
	if !ok {
		return content
	}
	var concat strings.Builder
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "text" {
			if s, ok := m["text"].(string); ok {
				concat.WriteString(s)
			}
		}
	}
	if concat.Len() == 0 {
		return content
	}
	body := concat.String()
	var parsed any
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		return parsed
	}
	return body
}

// parseSubmitFromText extracts the submit-tool input JSON the model
// emitted as its final assistant text. Accepts either:
//   - a bare object: {"<submit>": {...}}  — direct submission shape
//   - a fenced block surrounding such an object
//   - a bare object with the submit fields at the top level (when the
//     model omits the wrapper)
//
// Returns nil when nothing parseable is present, signalling the
// caller to surface the text as an error.
func parseSubmitFromText(text, submitToolName string) map[string]any {
	body := strings.TrimSpace(text)
	// Strip a leading ``` fence if present.
	if strings.HasPrefix(body, "```") {
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			body = body[nl+1:]
		}
		body = strings.TrimSuffix(body, "```")
		body = strings.TrimSpace(body)
	}
	// Find the first { and the last } — strip any surrounding prose.
	if first := strings.IndexByte(body, '{'); first >= 0 {
		if last := strings.LastIndexByte(body, '}'); last > first {
			body = body[first : last+1]
		}
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		slog.Debug("claude cli: submit parse failed", "err", err)
		return nil
	}
	// Pattern 1: {"submit_script": {...}}
	if inner, ok := raw[submitToolName].(map[string]any); ok {
		return inner
	}
	// Pattern 2: bare submit fields {"monty_script": "...", ...}
	if _, ok := raw["monty_script"]; ok {
		return raw
	}
	return nil
}
