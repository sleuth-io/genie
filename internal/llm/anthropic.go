package llm

import (
	"context"
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

	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 16000,
		System:    blocks,
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
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
