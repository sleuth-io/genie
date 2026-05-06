package eval

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sleuth-io/genie/internal/engine"
	"github.com/sleuth-io/genie/internal/plan"
)

// Runner wires the harness to the live executor. The Run* methods are the
// per-day measurements: hypothesis 1 (RunFirstCall), hypothesis 2
// (RunReplay), hypothesis 3 (RunAdversarial).
type Runner struct {
	Set       *Set
	Executor  *engine.Executor
	Generator *plan.Generator // optional; needed for token metrics
	Out       io.Writer
}

// CacheStatus classifies what happened on a single case run.
type CacheStatus int

const (
	StatusUnknown CacheStatus = iota
	StatusL1Hit              // verbatim re-run; no LLM call
	StatusL2Hit              // paraphrase; one normalize call, no full generate
	StatusGenerated          // full generate; both normalize + generate calls
)

func (s CacheStatus) String() string {
	switch s {
	case StatusL1Hit:
		return "L1"
	case StatusL2Hit:
		return "L2"
	case StatusGenerated:
		return "GEN"
	}
	return "?"
}

// CaseResult is one row in the result table.
type CaseResult struct {
	Case      Case
	Pass      bool
	Err       error // execution error, nil on success
	AssertErr error // assertion error, nil if assertions passed
	Duration  time.Duration
	Status    CacheStatus // L1Hit / L2Hit / Generated, derived from generator metrics
	LLMTokens int64       // total LLM tokens (input+output) attributed to this case
}

// Summary aggregates per-run metrics for the GO/KILL/REDESIGN call.
type Summary struct {
	Total      int
	Passed     int
	Failed     int
	Errored    int // execution-time errors (parse, run, MCP, LLM)
	L1Hits     int
	L2Hits     int
	Generated  int
	TotalTokens int64
	Wall       time.Duration // total wall-clock for the run
}

// PassRate is Passed / Total (as a fraction). 0 when Total == 0.
func (s Summary) PassRate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Passed) / float64(s.Total)
}

// RunFirstCall executes every case in the set against the live executor
// (canonical + every paraphrase). The caller is expected to start with a
// clean ./crystallized/ directory if they want true cold-cache numbers.
//
// Each case is executed once. Pass = (no execution error) AND (assertions
// hold).
func (r *Runner) RunFirstCall(ctx context.Context) ([]CaseResult, Summary, error) {
	return r.runAll(ctx, r.Set.AllCases(), "first-call")
}

// RunReplay executes every case in the set against an already-warm cache.
// Verbatim re-runs of yesterday's intents should L1-hit (zero LLM); the
// L2 path (normalize-only) shows up only on never-before-seen literals
// whose canonical form is in the cache.
//
// Hypothesis 2 success: pass rate ≥95% AND average tokens-per-case at
// least 10× lower than the cold-cache average.
func (r *Runner) RunReplay(ctx context.Context) ([]CaseResult, Summary, error) {
	return r.runAll(ctx, r.Set.AllCases(), "replay")
}

func (r *Runner) runAll(ctx context.Context, cases []Case, label string) ([]CaseResult, Summary, error) {
	results := make([]CaseResult, 0, len(cases))
	summary := Summary{Total: len(cases)}
	startAll := time.Now()

	for _, c := range cases {
		_, _ = fmt.Fprintf(r.Out, "→ %s/%s …", c.IntentID, c.Variant)
		res := r.runOne(ctx, c)
		results = append(results, res)
		summary.TotalTokens += res.LLMTokens
		switch res.Status {
		case StatusL1Hit:
			summary.L1Hits++
		case StatusL2Hit:
			summary.L2Hits++
		case StatusGenerated:
			summary.Generated++
		}

		switch {
		case res.Err != nil:
			summary.Errored++
			summary.Failed++
			_, _ = fmt.Fprintf(r.Out, " ERROR [%s tok=%d] (%s) [%v]\n",
				res.Status, res.LLMTokens, res.Err, res.Duration.Round(time.Millisecond))
		case res.AssertErr != nil:
			summary.Failed++
			_, _ = fmt.Fprintf(r.Out, " FAIL  [%s tok=%d] (%s) [%v]\n",
				res.Status, res.LLMTokens, res.AssertErr, res.Duration.Round(time.Millisecond))
		default:
			summary.Passed++
			_, _ = fmt.Fprintf(r.Out, " PASS  [%s tok=%d] [%v]\n",
				res.Status, res.LLMTokens, res.Duration.Round(time.Millisecond))
		}
	}

	summary.Wall = time.Since(startAll)
	r.printSummary(label, summary)
	return results, summary, nil
}

func (r *Runner) runOne(ctx context.Context, c Case) CaseResult {
	res := CaseResult{Case: c}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	parsed, err := engine.Parse(c.Query)
	if err != nil {
		res.Err = fmt.Errorf("parse: %w", err)
		return res
	}

	var before plan.Metrics
	if r.Generator != nil {
		before = r.Generator.Snapshot()
	}

	out, err := r.Executor.Execute(ctx, parsed)

	if r.Generator != nil {
		after := r.Generator.Snapshot()
		res.Status, res.LLMTokens = classify(before, after)
	}

	if err != nil {
		res.Err = err
		return res
	}
	if err := c.Assertions.Check(out); err != nil {
		res.AssertErr = err
		return res
	}
	res.Pass = true
	return res
}

// classify diffs Generator.Metrics before/after a single Execute and
// derives whether the case L1-hit, L2-hit, or generated.
func classify(before, after plan.Metrics) (CacheStatus, int64) {
	dn := after.NormalizeCalls - before.NormalizeCalls
	dg := after.GenerateCalls - before.GenerateCalls
	dToks := after.TotalLLMTokens() - before.TotalLLMTokens()

	switch {
	case dn == 0 && dg == 0:
		return StatusL1Hit, dToks
	case dn > 0 && dg == 0:
		return StatusL2Hit, dToks
	default:
		return StatusGenerated, dToks
	}
}

func (r *Runner) printSummary(label string, s Summary) {
	_, _ = fmt.Fprintf(r.Out, "\n--- %s summary ---\n", label)
	_, _ = fmt.Fprintf(r.Out, "  total:    %d\n", s.Total)
	_, _ = fmt.Fprintf(r.Out, "  passed:   %d\n", s.Passed)
	_, _ = fmt.Fprintf(r.Out, "  failed:   %d (incl. %d errored)\n", s.Failed, s.Errored)
	_, _ = fmt.Fprintf(r.Out, "  rate:     %.1f%%\n", s.PassRate()*100)
	_, _ = fmt.Fprintf(r.Out, "  L1 hits:  %d   L2 hits: %d   generated: %d\n",
		s.L1Hits, s.L2Hits, s.Generated)
	avg := float64(0)
	if s.Total > 0 {
		avg = float64(s.TotalTokens) / float64(s.Total)
	}
	_, _ = fmt.Fprintf(r.Out, "  tokens:   total=%d  avg/case=%.0f\n", s.TotalTokens, avg)
	_, _ = fmt.Fprintf(r.Out, "  wall:     %v\n", s.Wall.Round(time.Millisecond))
}

