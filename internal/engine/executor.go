package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sleuth-io/genie/internal/runtime"
)

// ScriptResolver returns the monty script source registered against a node
// shape. ok=false signals a cache miss. The Executor falls back to a
// ScriptGenerator (set via WithGenerator) on miss; if no generator is set,
// the miss surfaces as a hard error.
type ScriptResolver interface {
	Resolve(shape Shape) (script string, ok bool)
}

// ScriptGenerator produces a monty script for a node and the per-node rename
// rules the executor must apply around it. Implementations are expected to
// persist the result back into whatever ScriptResolver they're paired with —
// the executor does not write through. The Node carries the literal arg
// values for the current request, but generators should treat the shape
// (n.Shape()) as the cache key.
//
// The returned NodeRename may be nil — meaning the script's I/O uses the
// literal field names directly (e.g. when a previous run with the same
// literal shape generated the script). Otherwise the executor:
//   - applies ArgsLiteralToCanonical when building the kwargs map for the
//     script (literal user args → canonical names the script expects);
//   - applies ChildrenLiteralToCanonical when projecting children of THIS
//     node from the canonical-keyed parent the script returned.
type ScriptGenerator interface {
	Generate(ctx context.Context, n *Node, parent any) (script string, rename *NodeRename, err error)
}

// NodeRename captures per-node literal↔canonical mappings derived from the
// LLM's normalize call. nil means "no rename needed at this node".
type NodeRename struct {
	// ArgsLiteralToCanonical: literal arg name → canonical arg name. The
	// script-side knows only canonical names.
	ArgsLiteralToCanonical map[string]string

	// ChildrenLiteralToCanonical: literal child field name → canonical child
	// field name. Used to project children out of the canonical-keyed parent
	// returned by THIS node's script.
	ChildrenLiteralToCanonical map[string]string
}

// canonicalChildName returns the canonical name for a literal child name,
// falling back to the literal name when no rename is specified. Safe on a
// nil NodeRename.
func (r *NodeRename) canonicalChildName(literal string) string {
	if r == nil {
		return literal
	}
	if c, ok := r.ChildrenLiteralToCanonical[literal]; ok {
		return c
	}
	return literal
}

// canonicalArgName returns the canonical arg name. Safe on nil.
func (r *NodeRename) canonicalArgName(literal string) string {
	if r == nil {
		return literal
	}
	if c, ok := r.ArgsLiteralToCanonical[literal]; ok {
		return c
	}
	return literal
}

// Executor walks a parsed Query and resolves each node by dispatching to a
// monty script. Per-query result memoization on (script_id, canonical_args)
// is built in — hot for nested queries with shared sub-objects.
type Executor struct {
	eng  *runtime.MontyEngine
	caps *runtime.Capabilities
	sr   ScriptResolver
	gen  ScriptGenerator
}

// NewExecutor wires the engine + capabilities (host functions) + script
// resolver into a reusable executor. The Capabilities object is shared
// across calls; safe as long as the caller doesn't mutate it concurrently.
func NewExecutor(eng *runtime.MontyEngine, caps *runtime.Capabilities, sr ScriptResolver) *Executor {
	return &Executor{eng: eng, caps: caps, sr: sr}
}

// WithGenerator attaches an LLM-backed (or otherwise) generator that the
// executor calls on resolver misses. Returns the executor for chaining.
func (e *Executor) WithGenerator(gen ScriptGenerator) *Executor {
	e.gen = gen
	return e
}

// Execute runs the Query and returns a `{aliasOrName: value, ...}` map
// matching the request shape. Memoization is per-call: a fresh map is
// created and dropped at end of Execute.
func (e *Executor) Execute(ctx context.Context, q *Query) (map[string]any, error) {
	memo := map[string]any{}
	out := map[string]any{}
	for _, n := range q.TopLevel {
		v, err := e.resolveNode(ctx, n, nil, nil, memo)
		if err != nil {
			return nil, err
		}
		out[n.AliasOrName()] = v
	}
	return out, nil
}

// resolveNode handles a single node. Three paths:
//
//  1. Scalar leaf with parent: project parent[canonicalName(Name)].
//  2. Object/list with registered script: run the script (with memo),
//     then walk children.
//  3. Cache miss with no LLM fallback: error.
//
// parentRename is the NodeRename returned by the parent's Generate call;
// used to translate THIS node's literal name into the canonical key the
// parent's script wrote into the parent dict. nil for top-level nodes.
func (e *Executor) resolveNode(
	ctx context.Context,
	n *Node,
	parent any,
	parentRename *NodeRename,
	memo map[string]any,
) (any, error) {
	if len(n.Selection) == 0 && parent != nil {
		if obj, ok := parent.(map[string]any); ok {
			return obj[parentRename.canonicalChildName(n.Name)], nil
		}
	}

	shape := n.Shape()
	hash := shape.L1Hash()

	var (
		src    string
		rename *NodeRename
	)

	// When a generator is wired, ALWAYS go through it. The generator owns
	// the L1/L2/GEN decision and returns the rename appropriate to the
	// chosen path (nil rename only for tests using the static resolver).
	switch {
	case e.gen != nil:
		gSrc, gRename, err := e.gen.Generate(ctx, n, parent)
		if err != nil {
			return nil, fmt.Errorf("generate script for %q (shape=%s): %w", n.Name, hash, err)
		}
		src = gSrc
		rename = gRename
	default:
		// Test path: no generator, fall back to a static resolver. Rename
		// stays nil — these tests use literal-keyed scripts.
		cached, ok := e.sr.Resolve(shape)
		if !ok {
			return nil, fmt.Errorf("no script registered for shape %s (field=%q) and no generator wired",
				hash, n.Name)
		}
		src = cached
	}

	// Build args, translating literal arg names to canonical for the script.
	argMap := map[string]any{}
	for _, a := range n.Args {
		argMap[rename.canonicalArgName(a.Name)] = a.Value
	}

	memoKey := hash + "|" + canonicalArgsHash(argMap)
	if v, hit := memo[memoKey]; hit {
		return v, nil
	}

	mod, err := e.eng.Compile(src)
	if err != nil {
		return nil, fmt.Errorf("compile script for %q: %w", n.Name, err)
	}
	raw, _, err := e.eng.Run(ctx, mod, "execute",
		map[string]any{"args": argMap, "parent": parent}, e.caps)
	if err != nil {
		return nil, fmt.Errorf("run script for %q: %w", n.Name, err)
	}

	composed, err := e.composeChildren(ctx, n, raw, rename, memo)
	if err != nil {
		return nil, err
	}
	memo[memoKey] = composed
	return composed, nil
}

// composeChildren walks the selection set on every result element. Lists
// are mapped element-wise; single objects are mapped once; scalars are
// passed through. Each child receives `ownRename` as its parentRename so
// scalar leaves can project from the canonical-keyed item.
func (e *Executor) composeChildren(
	ctx context.Context,
	n *Node,
	raw any,
	ownRename *NodeRename,
	memo map[string]any,
) (any, error) {
	if len(n.Selection) == 0 {
		return raw, nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			one, err := e.composeOne(ctx, n, item, ownRename, memo)
			if err != nil {
				return nil, err
			}
			out = append(out, one)
		}
		return out, nil
	case map[string]any:
		return e.composeOne(ctx, n, v, ownRename, memo)
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("composition: %q has selection but script returned %T", n.Name, raw)
	}
}

func (e *Executor) composeOne(
	ctx context.Context,
	n *Node,
	parent any,
	parentRename *NodeRename,
	memo map[string]any,
) (any, error) {
	obj := map[string]any{}
	for _, child := range n.Selection {
		// For object children, descend into the canonical-keyed sub-object.
		// For scalar leaves, the leaf branch in resolveNode handles the
		// rename projection.
		var childParent = parent
		if len(child.Selection) > 0 {
			if pmap, ok := parent.(map[string]any); ok {
				childParent = pmap[parentRename.canonicalChildName(child.Name)]
			}
		}
		v, err := e.resolveNode(ctx, child, childParent, parentRename, memo)
		if err != nil {
			return nil, err
		}
		obj[child.AliasOrName()] = v
	}
	return obj, nil
}

// canonicalArgsHash produces a stable hex hash for a string-keyed argument
// map. Used as the value half of the per-query memo key.
func canonicalArgsHash(args map[string]any) string {
	if len(args) == 0 {
		return "_"
	}
	// Sort keys; emit a JSON-with-sorted-keys form. encoding/json already
	// sorts map keys lexicographically for map[string]any, but only at the
	// top level — nested maps would also be sorted, so we're fine.
	b, err := json.Marshal(args)
	if err != nil {
		return "_marshal_err_"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// StaticResolver is the Day-3 hand-populated script registry. Scripts are
// added by exemplar query: the exemplar is parsed, its top-level node shape
// is hashed, and the script is stored under that hash. Tests then run any
// query whose top-level shape matches the exemplar's.
type StaticResolver struct {
	scripts map[string]string
}

// NewStaticResolver returns an empty StaticResolver.
func NewStaticResolver() *StaticResolver {
	return &StaticResolver{scripts: map[string]string{}}
}

// Register parses `exemplar`, takes the L1 hash of every top-level node's
// shape, and stores `script` against each. Most exemplars have one
// top-level field — if you pass a multi-field exemplar, the same script is
// registered for each, which is rarely what you want. Returns an error on
// parse failure.
func (s *StaticResolver) Register(exemplar string, script string) error {
	q, err := Parse(exemplar)
	if err != nil {
		return fmt.Errorf("register: parse exemplar: %w", err)
	}
	if len(q.TopLevel) == 0 {
		return errors.New("register: exemplar has no top-level fields")
	}
	for _, n := range q.TopLevel {
		s.scripts[n.Shape().L1Hash()] = script
	}
	return nil
}

// RegisterShape stores a script directly against an L1 hash. Useful when
// the script is for a sub-node whose shape is known but doesn't have a
// natural exemplar query.
func (s *StaticResolver) RegisterShape(shape Shape, script string) {
	s.scripts[shape.L1Hash()] = script
}

// Resolve implements ScriptResolver.
func (s *StaticResolver) Resolve(shape Shape) (string, bool) {
	src, ok := s.scripts[shape.L1Hash()]
	return src, ok
}
