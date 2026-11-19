package fraud

import "testing"

func TestAssess(t *testing.T) {
	cases := []struct {
		name string
		in   AssessInput
		want string
	}{
		{"no data", AssessInput{}, RiskUnknown},
		{"too few", AssessInput{Delivered: 1, Cancelled: 1}, RiskUnknown},
		{"clean", AssessInput{Delivered: 9, Cancelled: 1}, RiskLow},
		{"borderline medium", AssessInput{Delivered: 7, Cancelled: 3}, RiskMedium},
		{"mostly cancelled", AssessInput{Delivered: 1, Cancelled: 4}, RiskHigh},
		{"failed twice", AssessInput{Delivered: 10, Failed: 2}, RiskMedium},
		{"external counts", AssessInput{HasExternal: true, ExtTotal: 10, ExtSuccess: 9, ExtCancel: 1}, RiskLow},
		{"rating only high", AssessInput{HasExternal: true, ExtRisk: "very_high"}, RiskHigh},
		{"rating only low", AssessInput{HasExternal: true, ExtRisk: "low"}, RiskLow},
		{"rating lifts level", AssessInput{Delivered: 10, HasExternal: true, ExtRisk: "high"}, RiskHigh},
		{"new customer rating stays unknown", AssessInput{HasExternal: true, ExtRisk: "unknown"}, RiskUnknown},
	}
	for _, c := range cases {
		if got := Assess(c.in); got.Level != c.want {
			t.Errorf("%s: got %s want %s (%+v)", c.name, got.Level, c.want, got)
		}
	}
	if a := Assess(AssessInput{Delivered: 3}); a.SuccessRate == nil || *a.SuccessRate != 100 {
		t.Errorf("success rate %+v", a.SuccessRate)
	}
}
