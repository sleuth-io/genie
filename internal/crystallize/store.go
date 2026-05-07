// Package crystallize persists generated monty scripts under ./crystallized/,
// keyed by node-shape hashes.
//
// Two-level layout (Day 6+):
//
//	./crystallized/l1/<sha256>.json   — alias: { canonical_hash, ... }
//	./crystallized/l2/<sha256>.json   — entry: { shape, monty_script, ... }
//
// Lookup:
//
//	L1 hit (literal-shape re-run): one disk read; no LLM call.
//	L1 miss → normalize call → L2 hit (paraphrase): write L1 alias for next time.
//	L1 + L2 miss → full plan generation; write L2 entry + L1 alias.
//
// Hypothesis-2 measurement leans on this: replays should land at L1 (zero
// tokens) or L2 (small normalize token cost) depending on whether the
// literal shape repeated or only the canonical did.
package crystallize

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sleuth-io/genie/internal/engine"
)

// Entry is the canonical-keyed record. One per unique canonical_schema.
type Entry struct {
	Shape           engine.Shape    `json:"shape"`
	Field           string          `json:"field"`
	CanonicalSchema json.RawMessage `json:"canonical_schema,omitempty"`
	CanonicalHash   string          `json:"canonical_hash"`
	MontyScript     string          `json:"monty_script"`
	IOSchema        any             `json:"io_schema,omitempty"`
	// Fixtures captures the (tool, args, response) tuples the LLM
	// observed during the GENERATE tool-use exploration phase. They
	// are NOT consulted at runtime — the cached script calls upstream
	// for fresh data — but are preserved on the entry so that when a
	// future Regenerate kicks in (e.g. upstream API drift), the LLM
	// can be primed with the original observed shapes rather than
	// re-exploring from scratch. Empty for entries written by paths
	// that don't use tool-use (synthesize, legacy single-turn
	// GENERATE).
	Fixtures json.RawMessage `json:"fixtures,omitempty"`
	// ExpectedOutput is the answer the LLM CLAIMED its script would
	// produce — used by the verification step to deep-diff the
	// script's actual output. Stored on success so future
	// regenerations can pin the same expectation.
	ExpectedOutput any       `json:"expected_output,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Alias is the literal-shape-keyed pointer record. Multiple aliases can
// point at the same Entry (paraphrases and verbatim repeats).
//
// Rename captures the per-literal mapping that the engine applies around
// the canonical-keyed script. Persisted so verbatim re-runs (L1 hits) skip
// the normalize call entirely.
type Alias struct {
	CanonicalHash string             `json:"canonical_hash"`
	Rename        *engine.NodeRename `json:"rename,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
}

// Store reads and writes Alias/Entry files under a root directory.
type Store struct {
	Root string
}

// NewStore returns a Store rooted at `root` (typically "./crystallized").
func NewStore(root string) *Store {
	return &Store{Root: root}
}

// ResolveLiteral returns the cached script for a literal shape via the
// L1 alias path, doing two cheap disk reads. Returns ("", false) on a clean
// miss.
func (s *Store) ResolveLiteral(shape engine.Shape) (script string, ok bool) {
	alias, ok, err := s.GetAlias(shape.L1Hash())
	if err != nil || !ok {
		return "", false
	}
	entry, ok, err := s.GetEntry(alias.CanonicalHash)
	if err != nil || !ok {
		return "", false
	}
	return entry.MontyScript, true
}

// ResolveCanonical returns the cached script for a known canonical hash.
// Used after a normalize call.
func (s *Store) ResolveCanonical(canonicalHash string) (script string, entry *Entry, ok bool) {
	e, ok, err := s.GetEntry(canonicalHash)
	if err != nil || !ok {
		return "", nil, false
	}
	return e.MontyScript, e, true
}

// GetAlias reads an L1 alias file. Missing → (nil, false, nil).
func (s *Store) GetAlias(hash string) (*Alias, bool, error) {
	path := s.l1Path(hash)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", path, err)
	}
	return &alias, true, nil
}

// GetEntry reads an L2 entry file. Missing → (nil, false, nil).
func (s *Store) GetEntry(canonicalHash string) (*Entry, bool, error) {
	path := s.l2Path(canonicalHash)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", path, err)
	}
	return &entry, true, nil
}

// PutAlias writes an L1 alias mapping a literal-shape hash to a canonical
// hash, plus the rename rules the engine must apply around the cached
// script for this particular literal phrasing. Idempotent — overwrites.
func (s *Store) PutAlias(literalHash, canonicalHash string, rename *engine.NodeRename) error {
	alias := Alias{
		CanonicalHash: canonicalHash,
		Rename:        rename,
		CreatedAt:     time.Now().UTC(),
	}
	return s.writeJSON(s.l1Path(literalHash), alias)
}

// PutEntry writes an L2 entry keyed by canonical hash. Idempotent.
func (s *Store) PutEntry(entry Entry) error {
	if entry.CanonicalHash == "" {
		return errors.New("PutEntry: empty canonical_hash")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	return s.writeJSON(s.l2Path(entry.CanonicalHash), entry)
}

// Resolve implements engine.ScriptResolver — used by the executor's first
// cheap-lookup pass. Only checks the L1 alias path. The slower normalize→L2
// path is the generator's job, kicked off on miss.
func (s *Store) Resolve(shape engine.Shape) (string, bool) {
	return s.ResolveLiteral(shape)
}

func (s *Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

func (s *Store) l1Path(hash string) string {
	return filepath.Join(s.Root, "l1", hash+".json")
}

func (s *Store) l2Path(hash string) string {
	return filepath.Join(s.Root, "l2", hash+".json")
}
