package fixtures

import (
	"strings"
	"testing"
)

func TestDiff_FloatNoiseIsMatch(t *testing.T) {
	// Real case from the session log: one-ULP drift on a sum that
	// re-ordered between LLM-claim time and replay time.
	if diff := Diff(9.313641864960575, 9.313641864960577, ""); diff != "" {
		t.Errorf("expected ULP-level float drift to match, got diff: %q", diff)
	}
}

func TestDiff_RealNumericDivergenceTrips(t *testing.T) {
	// Also from the session log: 0.6% diff on a sum. This is real
	// arithmetic divergence (not FP noise) and must still be flagged.
	if diff := Diff(1663.18, 1673.6383749999998, ""); diff == "" {
		t.Error("expected 0.6% diff to be flagged as a divergence")
	}
}

func TestDiff_IntegersExactWithinFloatBranch(t *testing.T) {
	if diff := Diff(100, 100, ""); diff != "" {
		t.Errorf("equal ints should match, got %q", diff)
	}
	if diff := Diff(100, 101, ""); diff == "" {
		t.Error("100 vs 101 should be flagged")
	}
}

func TestDiff_NearZero(t *testing.T) {
	// Below absolute floor — match.
	if diff := Diff(0.0, 1e-15, ""); diff != "" {
		t.Errorf("near-zero values should match, got %q", diff)
	}
	// Real value vs zero — mismatch.
	if diff := Diff(0.0, 0.01, ""); diff == "" {
		t.Error("0 vs 0.01 should be flagged")
	}
}

func TestDiff_FloatVsInt(t *testing.T) {
	// JSON round-trip can give either; both numeric, exact equality.
	if diff := Diff(42, 42.0, ""); diff != "" {
		t.Errorf("42 vs 42.0 should match, got %q", diff)
	}
}

func TestDiff_NestedFloatNoise(t *testing.T) {
	// Tolerance applies inside maps and lists too.
	expected := map[string]any{"total": 9.313641864960575}
	actual := map[string]any{"total": 9.313641864960577}
	if diff := Diff(expected, actual, ""); diff != "" {
		t.Errorf("nested float drift should match, got %q", diff)
	}
}

func TestDiff_BooleansStillExact(t *testing.T) {
	if diff := Diff(true, false, ""); diff == "" {
		t.Error("true vs false should be flagged")
	}
	if diff := Diff(true, true, ""); diff != "" {
		t.Errorf("equal booleans should match, got %q", diff)
	}
}

func TestDiff_DivergenceMessageMentionsBothValues(t *testing.T) {
	diff := Diff(1.0, 2.0, "")
	if !strings.Contains(diff, "expected 1") || !strings.Contains(diff, "got 2") {
		t.Errorf("diff message should name both values, got %q", diff)
	}
}
