package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sleuth-io/genie/internal/session"
	"github.com/sleuth-io/genie/pkg/genie"
)

// Scenario is one provider-tagged smoke check. Distinct from Intent
// (which is used by the spike's hypothesis tests against a fixed
// programmatic GitHub provider): scenarios run against the user's
// real ~/.config/genie/config.json, so they only execute when the
// named provider is actually configured. Missing providers SKIP the
// scenario rather than fail it — keeps the smoke-suite useful on
// a partially-configured machine.
type Scenario struct {
	ID          string     `yaml:"id"`
	Description string     `yaml:"description"`
	Provider    string     `yaml:"provider"`
	Query       string     `yaml:"query"`
	Assertions  Assertions `yaml:"assertions"`
}

// ScenarioSet is the parsed eval/scenarios.yaml file.
type ScenarioSet struct {
	Scenarios []Scenario `yaml:"scenarios"`
}

// LoadScenarios reads and parses a scenarios YAML file.
func LoadScenarios(path string) (*ScenarioSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var set ScenarioSet
	if err := yaml.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &set, nil
}

// ScenarioOutcome is one row of the run-result table.
type ScenarioOutcome struct {
	Scenario Scenario
	Status   string // "PASS" | "FAIL" | "SKIP"
	Reason   string // failure or skip reason
	Duration time.Duration
}

// ScenarioSummary aggregates the run.
type ScenarioSummary struct {
	Total    int
	Passed   int
	Failed   int
	Skipped  int
	Outcomes []ScenarioOutcome
}

// RunScenarios executes each scenario against the supplied Genie
// instance, skipping any whose Provider isn't in the configured
// registry. Out is the writer for human-readable progress.
//
// Returns the summary; a non-zero Failed count is the caller's
// signal to exit 1.
func RunScenarios(ctx context.Context, g *genie.Genie, set *ScenarioSet, out io.Writer) ScenarioSummary {
	configured := map[string]struct{}{}
	for _, name := range g.ProviderNames() {
		configured[name] = struct{}{}
	}

	summary := ScenarioSummary{Total: len(set.Scenarios)}
	for _, sc := range set.Scenarios {
		o := runOneScenario(ctx, g, sc, configured)
		summary.Outcomes = append(summary.Outcomes, o)
		switch o.Status {
		case "PASS":
			summary.Passed++
			_, _ = fmt.Fprintf(out, "  PASS  %-40s [%v]\n", sc.ID, o.Duration.Round(time.Millisecond))
		case "FAIL":
			summary.Failed++
			_, _ = fmt.Fprintf(out, "  FAIL  %-40s %s [%v]\n", sc.ID, o.Reason, o.Duration.Round(time.Millisecond))
		case "SKIP":
			summary.Skipped++
			_, _ = fmt.Fprintf(out, "  SKIP  %-40s %s\n", sc.ID, o.Reason)
		}
	}
	_, _ = fmt.Fprintf(out, "\n%d scenarios: %d passed, %d failed, %d skipped\n",
		summary.Total, summary.Passed, summary.Failed, summary.Skipped)
	return summary
}

func runOneScenario(ctx context.Context, g *genie.Genie, sc Scenario, configured map[string]struct{}) ScenarioOutcome {
	if sc.Provider == "" {
		return ScenarioOutcome{Scenario: sc, Status: "FAIL", Reason: "scenario missing required field: provider"}
	}
	if _, ok := configured[sc.Provider]; !ok {
		return ScenarioOutcome{
			Scenario: sc, Status: "SKIP",
			Reason: fmt.Sprintf("provider %q not configured (configured: %v)", sc.Provider, keysOf(configured)),
		}
	}

	// Tag this scenario's events with a fresh query_id so the
	// behavioural-assertion pass can pick out the right slice of the
	// session log regardless of what else is firing concurrently.
	queryID := session.NewQueryID()
	scenarioCtx := session.WithQueryID(ctx, queryID)

	start := time.Now()
	result, err := g.QueryMap(scenarioCtx, sc.Provider, sc.Query)
	dur := time.Since(start)
	if err != nil {
		return ScenarioOutcome{Scenario: sc, Status: "FAIL", Reason: "query: " + err.Error(), Duration: dur}
	}
	if err := sc.Assertions.Check(result); err != nil {
		return ScenarioOutcome{Scenario: sc, Status: "FAIL", Reason: "result: " + err.Error(), Duration: dur}
	}

	// Behavioural assertions: read the session log, filter to this
	// scenario's records by query_id, run the checks. Failures here
	// are real signal — incorrectness, latency regression, or an
	// LLM-script anti-pattern.
	records, _ := session.ReadByQueryID(g.SessionPath(), queryID)
	if err := sc.Assertions.CheckBehavior(records, dur); err != nil {
		return ScenarioOutcome{Scenario: sc, Status: "FAIL", Reason: "behavior: " + err.Error(), Duration: dur}
	}

	return ScenarioOutcome{Scenario: sc, Status: "PASS", Duration: dur}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ErrNoScenarios signals an empty file (caller decides whether
// that's an error or a "nothing to do" success).
var ErrNoScenarios = errors.New("no scenarios in set")
