package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

	args := []string{
		"-p", prompt.String(),
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", "",
	}
	model := ModelFromContext(ctx)
	if model == "" {
		model = c.model
	}
	if model != "" {
		args = append(args, "--model", model)
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
