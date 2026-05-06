package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Shape is the hashable structural identity of a Node — what fields it
// requests, what arg names it carries, and the recursive shape of its
// selection set. Crucially, Shape excludes:
//
//   - Aliases (cosmetic, controlled by caller)
//   - Argument values (runtime data, not script structure)
//   - Source positions, comments, etc.
//
// Two queries with the same Shape can run the same monty script — that's
// the load-bearing claim of L1 caching. Differs from canonicalised Shape
// (Day 6 / L2) in that field names are *not* normalised: `pull_requests`
// and `pullRequests` produce different L1 hashes.
type Shape struct {
	Field    string  `json:"field"`
	ArgNames []string `json:"args"`
	Children []Shape  `json:"selection"`
}

// Shape derives the hashable Shape of a Node. Children and ArgNames are
// sorted in canonical order so that the hash is invariant under source
// re-ordering.
func (n *Node) Shape() Shape {
	args := make([]string, 0, len(n.Args))
	for _, a := range n.Args {
		args = append(args, a.Name)
	}
	sort.Strings(args)

	children := make([]Shape, 0, len(n.Selection))
	for _, c := range n.Selection {
		children = append(children, c.Shape())
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Field < children[j].Field
	})

	return Shape{
		Field:    n.Name,
		ArgNames: args,
		Children: children,
	}
}

// L1Hash returns the canonical SHA256 hex of the shape's JSON encoding.
// Stable across processes — no maps, no time-sensitive data, no Go-pointer
// addresses. Suitable for filesystem keying under crystallized/l1/.
func (s Shape) L1Hash() string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal can only fail here on values we don't construct
		// (channels, funcs, cycles); panic is fine and signals a programmer
		// error rather than an input one.
		panic("engine: marshal Shape: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
