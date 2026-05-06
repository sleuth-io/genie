// Package engine parses hallucinated GraphQL-shaped queries into a Node tree
// and resolves them by dispatching each node to a monty script keyed by its
// shape hash. The parser uses vektah/gqlparser/v2 in schemaless mode (no
// schema validation) — invented field names are passed through to the
// resolver as intent signals per FR-1.
package engine

import (
	"errors"
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Query is the parsed result of a single user-supplied GraphQL string. We
// only support unnamed query operations for now; mutations, subscriptions,
// fragments, and variables are out of scope for the spike (the PRD permits
// "regex + best-effort" — vektah is strictly more lenient).
type Query struct {
	// TopLevel are the children of the outermost selection set (the
	// `{ ... }` of an unnamed query). A request like `{ a b }` yields two
	// nodes; we resolve and compose them in order.
	TopLevel []*Node
}

// Node is one selected field. Mirrors ast.Field but strips the schema-
// validation fields (Definition, ObjectDefinition) which are nil in
// schemaless parsing anyway, and resolves Argument values into Go natives
// at parse time so downstream stages don't have to repeat the work.
type Node struct {
	// Alias is the response key the caller asked for. Empty when the
	// caller didn't supply an alias — fall back to Name. Composition uses
	// AliasOrName(); the shape hash deliberately ignores aliases.
	Alias string

	// Name is the GraphQL field name as written. May be hallucinated.
	Name string

	// Args is the literal argument map at this node, with values resolved
	// to Go natives (string, float64, bool, []any, map[string]any). Order
	// is preserved from the source for human-friendliness; the shape hash
	// canonicalises by sorting.
	Args []Argument

	// Selection is the child node list. Empty for scalar leaves.
	Selection []*Node
}

// Argument is one (name, value) pair on a Node. Value mirrors the value-
// kinds vektah produces: strings/numbers/booleans/lists/maps. Variables
// are not resolved (we don't accept variables in this spike).
type Argument struct {
	Name  string
	Value any
}

// AliasOrName returns the response key for this node — the alias if set,
// otherwise the field name.
func (n *Node) AliasOrName() string {
	if n.Alias != "" {
		return n.Alias
	}
	return n.Name
}

// Parse turns a GraphQL-shaped string into a Query. Returns an error only
// for syntax failures vektah can't recover from; unknown field names and
// invented arguments pass through silently — that's the entire point of
// the schemaless mode.
func Parse(query string) (*Query, error) {
	doc, err := parser.ParseQuery(&ast.Source{Name: "genie", Input: query})
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(doc.Operations) == 0 {
		return nil, errors.New("parse: query has no operations")
	}
	if len(doc.Operations) > 1 {
		return nil, errors.New("parse: multiple operations in one document not supported")
	}
	op := doc.Operations[0]
	if op.Operation != ast.Query && op.Operation != "" {
		return nil, fmt.Errorf("parse: only query operations supported, got %q", op.Operation)
	}

	out := &Query{}
	for _, sel := range op.SelectionSet {
		field, ok := sel.(*ast.Field)
		if !ok {
			return nil, fmt.Errorf("parse: top-level selections must be fields (got %T) — fragments unsupported", sel)
		}
		node, err := convertField(field)
		if err != nil {
			return nil, err
		}
		out.TopLevel = append(out.TopLevel, node)
	}
	return out, nil
}

func convertField(f *ast.Field) (*Node, error) {
	args := make([]Argument, 0, len(f.Arguments))
	for _, a := range f.Arguments {
		v, err := a.Value.Value(nil)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", a.Name, err)
		}
		args = append(args, Argument{Name: a.Name, Value: v})
	}

	children := make([]*Node, 0, len(f.SelectionSet))
	for _, sel := range f.SelectionSet {
		child, ok := sel.(*ast.Field)
		if !ok {
			return nil, fmt.Errorf("selection in %q: only fields supported, got %T", f.Name, sel)
		}
		c, err := convertField(child)
		if err != nil {
			return nil, err
		}
		children = append(children, c)
	}

	return &Node{
		Alias:     f.Alias,
		Name:      f.Name,
		Args:      args,
		Selection: children,
	}, nil
}
