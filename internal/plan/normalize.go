package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/mrdon/gqlspike/internal/engine"
)

// normalizeOutput is the JSON shape we ask Claude to produce on a normalize
// call.
type normalizeOutput struct {
	CanonicalSchema canonicalSchema   `json:"canonical_schema"`
	ArgRename       map[string]string `json:"arg_rename"`   // literal → canonical
	FieldRename     map[string]string `json:"field_rename"` // literal child name → canonical child name
}

// canonicalSchema is the recursive, hashable canonical shape. We re-marshal
// it on the Go side with sorted args + sorted children to guarantee
// byte-stability of the hash regardless of the LLM's output ordering.
type canonicalSchema struct {
	Field     string            `json:"field"`
	Args      []string          `json:"args"`
	Selection []canonicalSchema `json:"selection"`
}

// normalizeNode runs the small NORMALIZE LLM call for one node and returns:
//   - the canonical schema (re-serialised with stable ordering)
//   - its SHA256 hex hash (the L2 cache key)
//   - the per-call rename info (literal↔canonical for args + child fields)
func (g *Generator) normalizeNode(
	ctx context.Context,
	n *engine.Node,
) (json.RawMessage, string, *engine.NodeRename, *canonicalSchema, error) {
	user, err := buildNormalizeUserPrompt(n)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("build normalize user prompt: %w", err)
	}

	slog.Info("plan: normalize",
		"field", n.Name,
		"shape_hash", n.Shape().L1Hash()[:12],
	)

	resp, usage, err := g.callClaude(ctx, g.normalizeSystem, user)
	if err != nil {
		return nil, "", nil, nil, err
	}
	g.metrics.NormalizeCalls++
	g.metrics.NormalizeInputTokens += usage.InputTokens
	g.metrics.NormalizeOutputTokens += usage.OutputTokens
	g.metrics.CacheReadInputTokens += usage.CacheReadInputTokens
	g.metrics.CacheCreationInputTokens += usage.CacheCreationInputTokens

	out, err := parseNormalizeResponse(resp)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("parse normalize response: %w (raw=%q)", err, truncate(resp, 500))
	}

	canonRaw, hash := stableHash(out.CanonicalSchema)
	rename := &engine.NodeRename{
		ArgsLiteralToCanonical:     out.ArgRename,
		ChildrenLiteralToCanonical: out.FieldRename,
	}
	canonCopy := out.CanonicalSchema
	return canonRaw, hash, rename, &canonCopy, nil
}

func parseNormalizeResponse(text string) (*normalizeOutput, error) {
	body := stripCodeFence(text)

	var out normalizeOutput
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if out.CanonicalSchema.Field == "" {
		return nil, errors.New("missing canonical_schema.field")
	}
	if out.ArgRename == nil {
		out.ArgRename = map[string]string{}
	}
	if out.FieldRename == nil {
		out.FieldRename = map[string]string{}
	}
	return &out, nil
}

// stableHash recursively canonicalises the schema (sort args, sort children
// by field) and returns (re-serialised JSON, SHA256 hex). Two nodes whose
// canonical schemas differ only in arg/child ordering hash identically.
func stableHash(c canonicalSchema) (json.RawMessage, string) {
	c = canonicalize(c)
	buf, err := json.Marshal(c)
	if err != nil {
		// canonicalSchema only contains string + []string + []canonicalSchema —
		// json.Marshal can't fail. Panic so the bug is obvious if the type
		// ever drifts.
		panic("plan: marshal canonicalSchema: " + err.Error())
	}
	sum := sha256.Sum256(buf)
	return json.RawMessage(buf), hex.EncodeToString(sum[:])
}

func canonicalize(c canonicalSchema) canonicalSchema {
	args := append([]string(nil), c.Args...)
	sort.Strings(args)
	args = dedupSorted(args)

	children := make([]canonicalSchema, len(c.Selection))
	for i, ch := range c.Selection {
		children[i] = canonicalize(ch)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Field < children[j].Field })

	return canonicalSchema{
		Field:     c.Field,
		Args:      args,
		Selection: children,
	}
}

func dedupSorted(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// buildNormalizeUserPrompt is the per-call user message for normalize.
// Just the node's literal shape — no parent context, no arg values, no
// extra noise. Smaller user message → faster LLM call.
func buildNormalizeUserPrompt(n *engine.Node) (string, error) {
	shape := n.Shape()
	buf, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`Canonicalise this GraphQL node and emit the canonical_schema JSON only.

Literal shape (as written by the user):
%s

Now produce the JSON response.`, buf), nil
}

// Compile-time guard: the Anthropic Usage type stays available.
var _ anthropic.Usage = anthropic.Usage{}
