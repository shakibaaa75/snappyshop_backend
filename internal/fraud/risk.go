package fraud

import "fmt"

// Risk levels shown to sellers.
const (
	RiskLow     = "low"
	RiskMedium  = "medium"
	RiskHigh    = "high"
	RiskUnknown = "unknown"
)

// AssessInput is everything we know about one phone number.
type AssessInput struct {
	// Our own platform history (finished orders only).
	Delivered, Cancelled, Failed int
	// Third-party answer, when there is one.
	HasExternal                     bool
	ExtTotal, ExtSuccess, ExtCancel int
	ExtRating, ExtRisk              string // rating-based answers (Pathao)
}

// Assessment is the estimated risk and the plain-language reasons for it.
type Assessment struct {
	Level       string
	Sample      int      // orders the estimate is based on
	SuccessRate *float64 // percent; nil when there is not enough data
	Reasons     []string
}

// minSample is the fewest orders we will put a percentage on.
const minSample = 3

func rank(l string) int {
	switch l {
	case RiskLow:
		return 1
	case RiskMedium:
		return 2
	case RiskHigh:
		return 3
	}
	return 0
}

func atLeast(cur, floor string) string {
	if rank(floor) > rank(cur) {
		return floor
	}
	return cur
}

// Assess combines our history and the provider's counts. It is an estimate to
// help a seller decide, not a verdict on the customer.
func Assess(in AssessInput) Assessment {
	good := in.Delivered
	bad := in.Cancelled + in.Failed
	if in.HasExternal {
		good += in.ExtSuccess
		bad += in.ExtCancel
	}
	n := good + bad
	out := Assessment{Level: RiskUnknown, Sample: n}

	if n >= minSample {
		rate := float64(good) / float64(n) * 100
		out.SuccessRate = &rate
		switch badShare := float64(bad) / float64(n); {
		case badShare <= 0.15:
			out.Level = RiskLow
		case badShare <= 0.35:
			out.Level = RiskMedium
		default:
			out.Level = RiskHigh
		}
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d of %d past orders were delivered (%.0f%%)", good, n, rate))
	} else if n > 0 {
		out.Reasons = append(out.Reasons, fmt.Sprintf("Only %d past order(s) on record, too few to judge", n))
	} else {
		out.Reasons = append(out.Reasons, "No order history found for this number")
	}

	if in.Failed >= 2 {
		out.Level = atLeast(out.Level, RiskMedium)
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d failed deliveries", in.Failed))
	}
	if in.HasExternal {
		switch in.ExtRisk {
		case "high", "very_high":
			out.Level = atLeast(out.Level, RiskHigh)
			out.Reasons = append(out.Reasons, "Courier rating marks this customer as risky")
		case "medium":
			out.Level = atLeast(out.Level, RiskMedium)
			out.Reasons = append(out.Reasons, "Courier rating is only average")
		case "low":
			if out.Level == RiskUnknown {
				out.Level = RiskLow
				out.Reasons = append(out.Reasons, "Courier rating is good")
			}
		}
	}
	return out
}
