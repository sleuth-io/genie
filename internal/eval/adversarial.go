package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sleuth-io/genie/internal/engine"
	"github.com/sleuth-io/genie/internal/plan"
)

// AdversarialSet is the parsed eval/adversarial.yaml file.
type AdversarialSet struct {
	Pairs []Pair `yaml:"pairs"`
}

// Pair is a query pair the adversarial test runs through normalize.
// Expect is "different" (the load-bearing case) or "same" (control —
// paraphrases that MUST collide).
type Pair struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	A           string `yaml:"a"`
	B           string `yaml:"b"`
	Expect      string `yaml:"expect"` // "different" or "same"
}

// LoadAdversarial reads and parses an adversarial YAML file.
func LoadAdversarial(path string) (*AdversarialSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s AdversarialSet
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

// PairResult is one row.
type PairResult struct {
	Pair       Pair
	HashA      string
	HashB      string
	Collided   bool
	Pass       bool // true if collision matches expectation
	ErrA, ErrB error
	Duration   time.Duration
}

// AdversarialSummary aggregates the run.
type AdversarialSummary struct {
	Total             int
	ExpectDifferent   int
	ExpectSame        int
	FalsePositives    int // expect=different but collided
	FalseNegatives    int // expect=same   but did not collide
	Pass              int
	Errored           int
	Wall              time.Duration
	FalsePositiveRate float64 // FP / ExpectDifferent
}

// RunAdversarial normalizes both queries in each pair and reports collision
// vs expectation. Hypothesis 3 success: FalsePositiveRate < 5%.
func RunAdversarial(ctx context.Context, gen *plan.Generator, set *AdversarialSet, out io.Writer) (AdversarialSummary, []PairResult, error) {
	summary := AdversarialSummary{Total: len(set.Pairs)}
	results := make([]PairResult, 0, len(set.Pairs))
	startAll := time.Now()

	for _, p := range set.Pairs {
		_, _ = fmt.Fprintf(out, "→ %s …", p.ID)
		res := runOnePair(ctx, gen, p)
		results = append(results, res)

		switch p.Expect {
		case "different":
			summary.ExpectDifferent++
			if res.Collided {
				summary.FalsePositives++
			}
		case "same":
			summary.ExpectSame++
			if !res.Collided {
				summary.FalseNegatives++
			}
		}
		if res.ErrA != nil || res.ErrB != nil {
			summary.Errored++
			_, _ = fmt.Fprintf(out, " ERROR (a=%v b=%v) [%v]\n",
				res.ErrA, res.ErrB, res.Duration.Round(time.Millisecond))
			continue
		}
		if res.Pass {
			summary.Pass++
			_, _ = fmt.Fprintf(out, " PASS [%s] (collided=%v expected=%s) [%v]\n",
				shortHash(res.HashA), res.Collided, p.Expect, res.Duration.Round(time.Millisecond))
		} else {
			_, _ = fmt.Fprintf(out, " FAIL (collided=%v expected=%s) hashA=%s hashB=%s [%v]\n",
				res.Collided, p.Expect, shortHash(res.HashA), shortHash(res.HashB),
				res.Duration.Round(time.Millisecond))
		}
	}

	summary.Wall = time.Since(startAll)
	if summary.ExpectDifferent > 0 {
		summary.FalsePositiveRate = float64(summary.FalsePositives) / float64(summary.ExpectDifferent)
	}

	_, _ = fmt.Fprintf(out, "\n--- adversarial summary ---\n")
	_, _ = fmt.Fprintf(out, "  total:                %d\n", summary.Total)
	_, _ = fmt.Fprintf(out, "  expect different:     %d  (false-positive collisions: %d)\n",
		summary.ExpectDifferent, summary.FalsePositives)
	_, _ = fmt.Fprintf(out, "  expect same:          %d  (false-negative non-collisions: %d)\n",
		summary.ExpectSame, summary.FalseNegatives)
	_, _ = fmt.Fprintf(out, "  passed:               %d\n", summary.Pass)
	_, _ = fmt.Fprintf(out, "  errored:              %d\n", summary.Errored)
	_, _ = fmt.Fprintf(out, "  false-positive rate:  %.1f%% (target <5%%)\n", summary.FalsePositiveRate*100)
	_, _ = fmt.Fprintf(out, "  wall:                 %v\n", summary.Wall.Round(time.Millisecond))

	return summary, results, nil
}

func runOnePair(ctx context.Context, gen *plan.Generator, p Pair) PairResult {
	res := PairResult{Pair: p}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	hashA, errA := normalizeQuery(ctx, gen, p.A)
	hashB, errB := normalizeQuery(ctx, gen, p.B)
	res.HashA, res.HashB = hashA, hashB
	res.ErrA, res.ErrB = errA, errB
	if errA != nil || errB != nil {
		return res
	}

	res.Collided = hashA == hashB
	switch p.Expect {
	case "different":
		res.Pass = !res.Collided
	case "same":
		res.Pass = res.Collided
	}
	return res
}

// normalizeQuery parses the GraphQL query, takes the first top-level node,
// and returns its canonical hash. We assume single-top-level pairs in the
// adversarial set.
func normalizeQuery(ctx context.Context, gen *plan.Generator, q string) (string, error) {
	parsed, err := engine.Parse(q)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", q, err)
	}
	if len(parsed.TopLevel) != 1 {
		return "", fmt.Errorf("adversarial pair queries must have exactly one top-level field; got %d", len(parsed.TopLevel))
	}
	return gen.NormalizeOnly(ctx, parsed.TopLevel[0])
}

func shortHash(h string) string {
	if len(h) >= 12 {
		return h[:12]
	}
	return h
}
