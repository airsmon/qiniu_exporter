package billing

import (
	"math"
	"testing"
)

func TestFixed8MajorUnits(t *testing.T) {
	tests := []struct {
		name  string
		value Fixed8
		want  float64
	}{
		{name: "zero", value: 0, want: 0},
		{name: "smallest positive unit", value: 1, want: 0.00000001},
		{name: "fractional", value: 123456789, want: 1.23456789},
		{name: "negative", value: -123456789, want: -1.23456789},
		{name: "whole", value: 900000000, want: 9},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.value.MajorUnits()
			if math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("MajorUnits() = %.12f, want %.12f", got, test.want)
			}
		})
	}
}

func TestFixed8MajorUnitsHandlesInt64Limits(t *testing.T) {
	positive := Fixed8(math.MaxInt64).MajorUnits()
	negative := Fixed8(math.MinInt64).MajorUnits()
	if math.IsInf(positive, 0) || math.IsNaN(positive) || positive <= 0 {
		t.Fatalf("MaxInt64 conversion is not finite and positive: %v", positive)
	}
	if math.IsInf(negative, 0) || math.IsNaN(negative) || negative >= 0 {
		t.Fatalf("MinInt64 conversion is not finite and negative: %v", negative)
	}
}
