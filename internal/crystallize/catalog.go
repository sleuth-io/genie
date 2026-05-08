package crystallize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
)

// HashCatalog produces a stable fingerprint of an upstream provider's
// tool catalog. The hash covers everything that, if changed, would
// plausibly change what an LLM-generated script does:
//
//   - tool name (the script calls it by name)
//   - tool description (the LLM picks tools partly by description)
//   - input schema (script args)
//   - output schema (script response navigation)
//
// Deterministic across runs: tools are sorted by name, schemas are
// re-encoded canonically. The output is a 16-hex-char prefix of the
// sha256 — collision-resistant for any realistic catalog count and
// short enough to fit cleanly in a cache entry.
//
// Provider-neutral: works for any MCP server. The hash is stable as
// long as the upstream's tool surface is stable, regardless of
// provider identity or auth flow.
func HashCatalog(tools []mcp.Tool) string {
	type entry struct {
		Name         string          `json:"name"`
		Description  string          `json:"description,omitempty"`
		InputSchema  json.RawMessage `json:"input_schema,omitempty"`
		OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	}
	out := make([]entry, 0, len(tools))
	for _, t := range tools {
		var in, outSchema json.RawMessage
		if t.RawInputSchema != nil {
			in = canonicalJSON(t.RawInputSchema)
		} else if t.InputSchema.Type != "" {
			b, err := json.Marshal(t.InputSchema)
			if err == nil {
				in = canonicalJSON(b)
			}
		}
		if t.RawOutputSchema != nil {
			outSchema = canonicalJSON(t.RawOutputSchema)
		} else if t.OutputSchema.Type != "" {
			b, err := json.Marshal(t.OutputSchema)
			if err == nil {
				outSchema = canonicalJSON(b)
			}
		}
		out = append(out, entry{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  in,
			OutputSchema: outSchema,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	body, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}

// canonicalJSON re-encodes a JSON blob through encoding/json so map
// key ordering is deterministic across runs. encoding/json sorts map
// keys when marshalling — that's what makes this stable. If the
// input doesn't parse as JSON, the original bytes are returned (the
// hash will still be deterministic, just less robust to whitespace
// noise).
func canonicalJSON(in json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	out, err := json.Marshal(v)
	if err != nil {
		return in
	}
	return out
}
