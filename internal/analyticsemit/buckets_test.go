package analyticsemit

import (
	"math"
	"testing"
)

func TestRatioBucketBoundaries(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "<0.25x"},
		{0.24, "<0.25x"},
		{0.25, "0.25-0.5x"},
		{0.49, "0.25-0.5x"},
		{0.5, "0.5-1x"},
		{0.99, "0.5-1x"},
		{1, "1-2x"},
		{1.99, "1-2x"},
		{2, "2-4x"},
		{3.99, "2-4x"},
		{4, "4x+"},
		{100, "4x+"},
	}
	for _, c := range cases {
		if got := RatioBucket(c.in); got != c.want {
			t.Errorf("RatioBucket(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRatioBucketGuardsJunk(t *testing.T) {
	for _, in := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.5} {
		if got := RatioBucket(in); got != "unknown" {
			t.Errorf("RatioBucket(%v) = %q, want unknown", in, got)
		}
	}
}

func TestFilterCountBucketBoundaries(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"}, {-1, "0"},
		{1, "1-10"}, {10, "1-10"},
		{11, "11-25"}, {25, "11-25"},
		{26, "26-50"}, {50, "26-50"},
		{51, "51-100"}, {100, "51-100"},
		{101, "100+"},
	}
	for _, c := range cases {
		if got := FilterCountBucket(c.in); got != c.want {
			t.Errorf("FilterCountBucket(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
