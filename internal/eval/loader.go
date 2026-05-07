// Package eval is the curated-query test harness for the spike's three
// hypotheses (resolution feasibility, replay, fingerprinting).
//
// Day 5 wires the resolution-feasibility (hypothesis 1) measurement: every
// curated query is executed once against a cold cache and judged against
// shape-only assertions. The pass rate against the curated set is the
// load-bearing number for the GO/KILL/REDESIGN call on hypothesis 1.
package eval

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Set is the parsed eval/intents.yaml file.
type Set struct {
	Intents []Intent `yaml:"intents"`
}

// Intent is one user goal with a canonical query and a slate of paraphrases.
type Intent struct {
	ID          string       `yaml:"id"`
	Description string       `yaml:"description"`
	Query       string       `yaml:"query"`
	Assertions  Assertions   `yaml:"assertions"`
	Paraphrases []Paraphrase `yaml:"paraphrases,omitempty"`
}

// Paraphrase is one alternate phrasing of an Intent. It carries its own
// assertion set because paraphrases may use different alias / field names
// than the canonical query.
type Paraphrase struct {
	Query      string     `yaml:"query"`
	Assertions Assertions `yaml:"assertions"`
}

// Assertions describe the minimum shape a correct response must satisfy.
// Empty fields mean "no constraint of this kind".
type Assertions struct {
	// ListPaths: each path's value must be a non-empty list.
	ListPaths []string `yaml:"list_paths,omitempty"`

	// NonemptyPaths: each path must resolve to a non-null, non-zero value.
	NonemptyPaths []string `yaml:"nonempty_paths,omitempty"`

	// NonemptyPathsInEach: for each (listPath, [field…]) pair, every element
	// of the list at listPath must have all the listed fields non-null.
	NonemptyPathsInEach map[string][]string `yaml:"nonempty_paths_in_each,omitempty"`
}

// Load reads and parses an intents YAML file.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var set Set
	if err := yaml.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &set, nil
}

// AllCases flattens an intent slate into individual test cases (canonical
// + paraphrases). The harness iterates over the result.
type Case struct {
	IntentID   string
	Variant    string // "canonical" or "paraphrase[N]"
	Query      string
	Assertions Assertions
}

func (s *Set) AllCases() []Case {
	var out []Case
	for _, in := range s.Intents {
		out = append(out, Case{
			IntentID:   in.ID,
			Variant:    "canonical",
			Query:      in.Query,
			Assertions: in.Assertions,
		})
		for i, p := range in.Paraphrases {
			out = append(out, Case{
				IntentID:   in.ID,
				Variant:    fmt.Sprintf("paraphrase[%d]", i),
				Query:      p.Query,
				Assertions: p.Assertions,
			})
		}
	}
	return out
}
