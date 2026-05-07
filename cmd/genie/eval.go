package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/sleuth-io/genie/internal/envfile"
	"github.com/sleuth-io/genie/internal/eval"
	"github.com/sleuth-io/genie/pkg/genie"
)

// runEval executes the curated set in eval/intents.yaml. Two phases:
//
//	Phase 1 (always): wipe the cache if --cold, then run all cases.
//	                  This is the "first call" measurement (hypothesis 1).
//	Phase 2 (--replay): run all cases AGAIN against the now-warm cache.
//	                    This is the replay measurement (hypothesis 2).
//
// eval is the dev/CI entry point — not part of the user-facing CLI.
// It builds a single GitHub provider programmatically from the env,
// so it doesn't depend on a config file. It auto-loads ./.env if
// present so reproduction commands work without exporting tokens by
// hand; the rest of the binary uses normal process env only.
func runEval(ctx context.Context, args []string) error {
	if err := envfile.Load(); err != nil {
		slog.Warn("eval: failed loading .env (continuing with process env)", "err", err)
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	intentsPath := fs.String("intents", "eval/intents.yaml", "path to intents YAML")
	advPath := fs.String("adversarial", "eval/adversarial.yaml", "path to adversarial YAML")
	dir := fs.String("crystallized", "./crystallized", "crystallized cache directory")
	cold := fs.Bool("cold", false, "wipe crystallized dir before running (cold cache)")
	replay := fs.Bool("replay", false, "after the first run, run again against the warm cache")
	h3 := fs.Bool("hypothesis-3", false, "run only the adversarial fingerprint set (H3)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *h3 {
		return runHypothesis3(ctx, *advPath, *dir)
	}

	set, err := eval.Load(*intentsPath)
	if err != nil {
		return err
	}

	if *cold {
		if err := os.RemoveAll(*dir); err != nil {
			return fmt.Errorf("wipe %s: %w", *dir, err)
		}
		_, _ = fmt.Fprintf(os.Stderr, "wiped %s for cold-cache run\n", *dir)
	}

	g, err := newEvalGenie(ctx, *dir)
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()

	executor, _ := g.ExecutorFor("github")
	generator, _ := g.GeneratorFor("github")

	r := &eval.Runner{
		Set:       set,
		Executor:  executor,
		Generator: generator,
		Out:       os.Stdout,
	}
	_, firstSummary, err := r.RunFirstCall(ctx)
	if err != nil {
		return err
	}

	if !*replay {
		if firstSummary.PassRate() < 0.80 {
			os.Exit(2)
		}
		return nil
	}

	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintln(os.Stdout, "=== second pass (replay against warm cache) ===")
	_, replaySummary, err := r.RunReplay(ctx)
	if err != nil {
		return err
	}

	// Hypothesis 2 verdict: ≥95% pass AND ≥10× token reduction (avg/case).
	firstAvg := safeAvg(firstSummary.TotalTokens, firstSummary.Total)
	replayAvg := safeAvg(replaySummary.TotalTokens, replaySummary.Total)
	reduction := float64(0)
	if replayAvg > 0 {
		reduction = firstAvg / replayAvg
	} else if firstAvg > 0 {
		reduction = float64(1 << 30) // effectively infinite
	}

	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintln(os.Stdout, "=== hypothesis verdicts ===")
	_, _ = fmt.Fprintf(os.Stdout, "  H1 (≥80%% first-call):       %s (%.1f%%)\n",
		passFail(firstSummary.PassRate() >= 0.80), firstSummary.PassRate()*100)
	_, _ = fmt.Fprintf(os.Stdout, "  H2 (≥95%% replay correctness): %s (%.1f%%)\n",
		passFail(replaySummary.PassRate() >= 0.95), replaySummary.PassRate()*100)
	_, _ = fmt.Fprintf(os.Stdout, "  H2 (≥10× token reduction):   %s (%.1fx, %.0f → %.0f tokens/case)\n",
		passFail(reduction >= 10), reduction, firstAvg, replayAvg)

	if firstSummary.PassRate() < 0.80 || replaySummary.PassRate() < 0.95 || reduction < 10 {
		os.Exit(2)
	}
	return nil
}

func safeAvg(total int64, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(total) / float64(n)
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// runHypothesis3 runs the adversarial fingerprint set. Each pair is two
// queries that should canonicalise to either the same hash (paraphrase
// control) or different hashes (the load-bearing case). Pass = false-
// positive rate <5% on the expect=different pairs.
func runHypothesis3(ctx context.Context, advPath, dir string) error {
	set, err := eval.LoadAdversarial(advPath)
	if err != nil {
		return err
	}

	g, err := newEvalGenie(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()
	generator, _ := g.GeneratorFor("github")

	summary, _, err := eval.RunAdversarial(ctx, generator, set, os.Stdout)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintln(os.Stdout, "=== hypothesis verdict ===")
	_, _ = fmt.Fprintf(os.Stdout, "  H3 (<5%% false-positive collisions): %s (%.1f%%)\n",
		passFail(summary.FalsePositiveRate < 0.05), summary.FalsePositiveRate*100)
	if summary.FalsePositiveRate >= 0.05 {
		os.Exit(2)
	}
	return nil
}

// newEvalGenie builds a GitHub-only Genie programmatically from the
// process env. Bypasses the config-file requirement so the eval is
// reproducible without a user-level config in place.
func newEvalGenie(ctx context.Context, cacheDir string) (*genie.Genie, error) {
	token := os.Getenv("GITHUB_PERSONAL_ACCESS_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_PERSONAL_ACCESS_TOKEN not set")
	}
	return genie.New(ctx, genie.Config{
		Providers:    []genie.Provider{genie.GitHubMCP(token)},
		AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
		CacheDir:     cacheDir,
	})
}
