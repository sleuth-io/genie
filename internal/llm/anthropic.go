package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type anthropicClient struct {
	c anthropic.Client
}

// NewAnthropic builds a Client that calls the Anthropic API directly.
// If apiKey is empty the SDK reads ANTHROPIC_API_KEY from the process
// environment.
func NewAnthropic(apiKey string) Client {
	if apiKey != "" {
		return &anthropicClient{c: anthropic.NewClient(option.WithAPIKey(apiKey))}
	}
	return &anthropicClient{c: anthropic.NewClient()}
}

func (a *anthropicClient) Generate(ctx context.Context, system []SystemBlock, userText string) (Response, error) {
	blocks := make([]anthropic.TextBlockParam, 0, len(system))
	for _, b := range system {
		bp := anthropic.TextBlockParam{Text: b.Text}
		if b.CacheBreakAfter {
			bp.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		blocks = append(blocks, bp)
	}

	model := anthropic.Model(ModelFromContext(ctx))
	if model == "" {
		model = anthropic.Model(DefaultModel)
	}

	thinking := anthropic.ThinkingConfigParamUnion{
		OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
	}
	if EffortFromContext(ctx) == EffortDisabled {
		thinking = anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		}
	}

	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 16000,
		System:    blocks,
		Thinking:  thinking,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userText)),
		},
	}

	stream := a.c.Messages.NewStreaming(ctx, params)
	msg := anthropic.Message{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Response{}, fmt.Errorf("accumulate stream event: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return Response{}, fmt.Errorf("stream: %w", err)
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}

	return Response{
		Text: text.String(),
		Usage: Usage{
			InputTokens:         msg.Usage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
		},
	}, nil
}

// Chat drives one turn of a multi-turn tool-use conversation. Caller
// owns the message history: append the returned assistant turn (Text
// + ToolUses) plus a follow-up user message (carrying ToolResults
// for any ToolUses) before invoking again.
func (a *anthropicClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	systemBlocks := make([]anthropic.TextBlockParam, 0, len(req.System))
	for _, b := range req.System {
		bp := anthropic.TextBlockParam{Text: b.Text}
		if b.CacheBreakAfter {
			bp.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		systemBlocks = append(systemBlocks, bp)
	}

	model := anthropic.Model(ModelFromContext(ctx))
	if model == "" {
		model = anthropic.Model(DefaultModel)
	}

	thinking := anthropic.ThinkingConfigParamUnion{
		OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
	}
	if EffortFromContext(ctx) == EffortDisabled {
		thinking = anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		}
	}

	messages := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		conv, err := convertMessage(m)
		if err != nil {
			return ChatResponse{}, fmt.Errorf("convert message: %w", err)
		}
		messages = append(messages, conv)
	}

	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for _, t := range req.Tools {
		schema, err := convertSchema(t.InputSchema)
		if err != nil {
			return ChatResponse{}, fmt.Errorf("convert schema for tool %q: %w", t.Name, err)
		}
		tp := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tp})
	}

	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 16000,
		System:    systemBlocks,
		Thinking:  thinking,
		Messages:  messages,
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	stream := a.c.Messages.NewStreaming(ctx, params)
	msg := anthropic.Message{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return ChatResponse{}, fmt.Errorf("accumulate stream event: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("stream: %w", err)
	}

	resp := ChatResponse{
		StopReason: string(msg.StopReason),
		Usage: Usage{
			InputTokens:         msg.Usage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
		},
	}
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(b.Text)
		case anthropic.ToolUseBlock:
			input := map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			resp.ToolUses = append(resp.ToolUses, ToolUse{
				ID:    b.ID,
				Name:  b.Name,
				Input: input,
			})
		}
	}
	resp.Text = text.String()
	return resp, nil
}

// convertMessage flips llm.Message → anthropic.MessageParam. Handles
// the three valid combinations: user text, user tool-results,
// assistant text-and/or-tool-uses.
func convertMessage(m Message) (anthropic.MessageParam, error) {
	switch m.Role {
	case "user":
		if len(m.ToolResults) > 0 {
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.ToolResults)+1)
			if m.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Text))
			}
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropic.NewToolResultBlock(tr.ToolUseID, tr.Content, tr.IsError))
			}
			return anthropic.NewUserMessage(blocks...), nil
		}
		return anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)), nil
	case "assistant":
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.ToolUses)+1)
		if m.Text != "" {
			blocks = append(blocks, anthropic.NewTextBlock(m.Text))
		}
		for _, tu := range m.ToolUses {
			input, err := json.Marshal(tu.Input)
			if err != nil {
				return anthropic.MessageParam{}, fmt.Errorf("marshal tool_use input: %w", err)
			}
			blocks = append(blocks, anthropic.NewToolUseBlock(tu.ID, string(input), tu.Name))
		}
		return anthropic.NewAssistantMessage(blocks...), nil
	default:
		return anthropic.MessageParam{}, fmt.Errorf("unknown role %q", m.Role)
	}
}

// convertSchema marshals the tool's JSON-Schema map (as already
// captured from MCP) into the SDK's ToolInputSchemaParam shape. The
// SDK fixes the type to "object" via a constant default; we just
// thread properties + required through.
func convertSchema(schema map[string]any) (anthropic.ToolInputSchemaParam, error) {
	out := anthropic.ToolInputSchemaParam{}
	if props, ok := schema["properties"].(map[string]any); ok {
		out.Properties = props
	}
	if req, ok := schema["required"].([]any); ok {
		reqs := make([]string, 0, len(req))
		for _, v := range req {
			if s, ok := v.(string); ok {
				reqs = append(reqs, s)
			}
		}
		out.Required = reqs
	}
	return out, nil
}
