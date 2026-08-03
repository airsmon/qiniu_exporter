package billing

const fixed8Scale int64 = 100_000_000

// Fixed8 is a monetary value encoded by Qiniu as an integer with eight
// fractional decimal places.
type Fixed8 int64

// MajorUnits converts a Fixed8 value to the currency's major unit. Splitting
// the integer and fractional components avoids integer truncation and avoids
// converting the larger scaled integer to float64 in one step.
func (v Fixed8) MajorUnits() float64 {
	whole := int64(v) / fixed8Scale
	fraction := int64(v) % fixed8Scale
	return float64(whole) + float64(fraction)/float64(fixed8Scale)
}
