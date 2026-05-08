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
				// Unmarshal can leave input as nil if the model
				// emitted `"input": null` (common for tools with
				// no parameters). Re-init to empty map so the
				// caller-side echo doesn't serialize back as
				// "null", which the API rejects with
				// `tool_use.input: Input should be an object`.
				_ = json.Unmarshal(b.Input, &input)
				if input == nil {
					input = map[string]any{}
				}
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

// Drive runs the explore-then-submit loop end-to-end against the
// Anthropic SDK. Each turn the model can either call upstream tools
// (Drive routes the call through req.Executor and feeds the result
// back as the next turn's tool_result) or call req.SubmitToolName
// (Drive captures the input as LoopResult.Submit and ends).
//
// Per-tool observations are captured for verification replay. The
// model-facing tool name is preserved as Observation.ToolName — the
// caller (plan.Generator) handles any further translation.
//
// Bounded by 12 turns; matches plan/llm.go's previous handcrafted
// loop budget.
func (a *anthropicClient) Drive(ctx context.Context, req DriveRequest) (LoopResult, error) {
	const maxTurns = 12

	messages := append([]Message(nil), req.Messages...)
	res := LoopResult{}

	for turn := 0; turn < maxTurns; turn++ {
		chatResp, err := a.Chat(ctx, ChatRequest{
			System:   req.System,
			Messages: messages,
			Tools:    req.Tools,
		})
		if err != nil {
			return res, fmt.Errorf("chat turn %d: %w", turn, err)
		}
		res.Usage.InputTokens += chatResp.Usage.InputTokens
		res.Usage.OutputTokens += chatResp.Usage.OutputTokens
		res.Usage.CacheCreationTokens += chatResp.Usage.CacheCreationTokens
		res.Usage.CacheReadTokens += chatResp.Usage.CacheReadTokens
		res.StopReason = chatResp.StopReason

		if len(chatResp.ToolUses) == 0 {
			res.FinalText = chatResp.Text
			return res, nil
		}

		// Append the assistant turn so the next chat round has the
		// full conversation history.
		messages = append(messages, Message{
			Role:     "assistant",
			Text:     chatResp.Text,
			ToolUses: chatResp.ToolUses,
		})

		results := make([]ToolResult, 0, len(chatResp.ToolUses))
		var submittedThisTurn bool
		for _, tu := range chatResp.ToolUses {
			if tu.Name == req.SubmitToolName && req.SubmitToolName != "" {
				res.Submit = tu.Input
				submittedThisTurn = true
				results = append(results, ToolResult{
					ToolUseID: tu.ID,
					Content:   "received",
				})
				continue
			}
			if req.Executor == nil {
				results = append(results, ToolResult{
					ToolUseID: tu.ID,
					Content:   "no executor wired for tool " + tu.Name,
					IsError:   true,
				})
				continue
			}
			result, callErr := req.Executor.Call(ctx, tu.Name, tu.Input)
			if callErr != nil {
				results = append(results, ToolResult{
					ToolUseID: tu.ID,
					Content:   callErr.Error(),
					IsError:   true,
				})
				continue
			}
			res.Observations = append(res.Observations, Observation{
				ToolName: tu.Name,
				Args:     tu.Input,
				Result:   result,
			})
			payload, _ := json.Marshal(result)
			content := string(payload)
			if len(content) > 50000 {
				content = content[:50000] + "\n... [truncated]"
			}
			results = append(results, ToolResult{
				ToolUseID: tu.ID,
				Content:   content,
			})
		}

		if submittedThisTurn {
			return res, nil
		}

		messages = append(messages, Message{
			Role:        "user",
			ToolResults: results,
		})
	}

	return res, fmt.Errorf("tool-use loop hit %d turn limit without submit", maxTurns)
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
			// Anthropic's tool_use.input must be a JSON object,
			// never null. Coerce nil/empty inputs to {} so a
			// model that called a parameterless tool round-trips
			// safely on the next turn.
			//
			// NewToolUseBlock takes `any` and marshals it itself —
			// passing a string here would produce a JSON string
			// literal on the wire, not a JSON object. Pass the
			// Go map directly.
			inputVal := tu.Input
			if inputVal == nil {
				inputVal = map[string]any{}
			}
			blocks = append(blocks, anthropic.NewToolUseBlock(tu.ID, inputVal, tu.Name))
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
