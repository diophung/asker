package main

import (
	"math"
	"testing"
)

const eps = 1e-9

func almost(a, b float64) bool { return math.Abs(a-b) < eps }

func TestRecallAtK(t *testing.T) {
	j := Judged{"a": 1, "b": 1, "c": 1, "d": 1} // 4 relevant
	tests := []struct {
		name   string
		ranked []string
		k      int
		want   float64
	}{
		{"all in top-k", []string{"a", "b", "c", "d"}, 4, 1.0},
		{"half in top-k", []string{"a", "x", "b", "y"}, 4, 0.5},
		{"cutoff excludes late relevant", []string{"x", "y", "a", "b"}, 2, 0.0},
		{"cutoff includes early relevant", []string{"a", "b", "x", "y"}, 2, 0.5},
		{"no cutoff (k<=0)", []string{"x", "y", "z", "a", "b", "c", "d"}, 0, 1.0},
		{"empty results", nil, 10, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecallAtK(tc.ranked, j, tc.k); !almost(got, tc.want) {
				t.Fatalf("RecallAtK=%v want %v", got, tc.want)
			}
		})
	}
}

func TestRecallAtKNoRelevantIsNaN(t *testing.T) {
	if got := RecallAtK([]string{"a"}, Judged{}, 10); !math.IsNaN(got) {
		t.Fatalf("want NaN for empty judgments, got %v", got)
	}
}

func TestRecallDeduplicates(t *testing.T) {
	// A duplicate relevant doc must not push recall above the true fraction, and
	// must consume only one rank slot.
	j := Judged{"a": 1, "b": 1}
	if got := RecallAtK([]string{"a", "a", "a"}, j, 10); !almost(got, 0.5) {
		t.Fatalf("dup recall=%v want 0.5", got)
	}
}

func TestReciprocalRank(t *testing.T) {
	j := Judged{"rel": 1}
	tests := []struct {
		name   string
		ranked []string
		want   float64
	}{
		{"first", []string{"rel", "x", "y"}, 1.0},
		{"second", []string{"x", "rel", "y"}, 0.5},
		{"third", []string{"x", "y", "rel"}, 1.0 / 3.0},
		{"absent", []string{"x", "y", "z"}, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReciprocalRank(tc.ranked, j); !almost(got, tc.want) {
				t.Fatalf("RR=%v want %v", got, tc.want)
			}
		})
	}
}

func TestNDCGPerfectRankingIsOne(t *testing.T) {
	j := Judged{"a": 3, "b": 2, "c": 1}
	if got := NDCGAtK([]string{"a", "b", "c"}, j, 10); !almost(got, 1.0) {
		t.Fatalf("perfect nDCG=%v want 1.0", got)
	}
}

func TestNDCGBinaryKnownValue(t *testing.T) {
	// Binary relevance, one relevant doc at rank 2: DCG = 1/log2(3);
	// IDCG (ideal at rank 1) = 1/log2(2) = 1. nDCG = 1/log2(3) = 0.6309...
	j := Judged{"rel": 1}
	got := NDCGAtK([]string{"x", "rel", "y"}, j, 10)
	want := 1.0 / math.Log2(3)
	if !almost(got, want) {
		t.Fatalf("nDCG=%v want %v", got, want)
	}
}

func TestNDCGWorseThanIdealIsLess(t *testing.T) {
	j := Judged{"a": 3, "b": 2, "c": 1}
	ideal := NDCGAtK([]string{"a", "b", "c"}, j, 10)
	swapped := NDCGAtK([]string{"c", "b", "a"}, j, 10)
	if !(swapped < ideal) {
		t.Fatalf("swapped nDCG %v should be < ideal %v", swapped, ideal)
	}
	if swapped <= 0 || swapped >= 1 {
		t.Fatalf("swapped nDCG %v out of (0,1)", swapped)
	}
}

func TestNDCGNoRelevantIsZero(t *testing.T) {
	if got := NDCGAtK([]string{"a", "b"}, Judged{}, 10); got != 0 {
		t.Fatalf("nDCG with no relevant=%v want 0", got)
	}
}

func TestNDCGCutoffApplies(t *testing.T) {
	// Relevant doc sits at rank 3 but k=2 excludes it -> DCG 0 -> nDCG 0.
	j := Judged{"rel": 1}
	if got := NDCGAtK([]string{"x", "y", "rel"}, j, 2); got != 0 {
		t.Fatalf("nDCG@2=%v want 0 (relevant beyond cutoff)", got)
	}
}

func TestMeanIgnoringNaN(t *testing.T) {
	m, n := meanIgnoringNaN([]float64{1, math.NaN(), 3})
	if n != 2 || !almost(m, 2.0) {
		t.Fatalf("mean=%v n=%d want 2.0/2", m, n)
	}
	if _, n := meanIgnoringNaN([]float64{math.NaN()}); n != 0 {
		t.Fatalf("all-NaN should give n=0, got %d", n)
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(xs, 50); got != 50 {
		t.Fatalf("p50=%v want 50", got)
	}
	if got := percentile(xs, 90); got != 90 {
		t.Fatalf("p90=%v want 90", got)
	}
	if got := percentile(xs, 95); got != 100 {
		t.Fatalf("p95=%v want 100", got)
	}
	if got := percentile([]float64{42}, 95); got != 42 {
		t.Fatalf("single-element p95=%v want 42", got)
	}
	if got := percentile(nil, 50); !math.IsNaN(got) {
		t.Fatalf("empty percentile should be NaN, got %v", got)
	}
}
