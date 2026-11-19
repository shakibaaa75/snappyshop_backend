package uploads

// Limits per subscription plan. Stored images are lossless WebP, so they are
// larger than the originals; these numbers leave room for a full catalog.
type Quota struct {
	Bytes int64
	Files int
}

const mb = 1 << 20

// QuotaFor returns the storage quota for a plan ID ("free", "pro", "business").
// Unknown plans get the Free quota.
func QuotaFor(planID string) Quota {
	switch planID {
	case "business":
		return Quota{Bytes: 20 * 1024 * mb, Files: 12000}
	case "pro":
		return Quota{Bytes: 5 * 1024 * mb, Files: 4000}
	}
	return Quota{Bytes: 500 * mb, Files: 400}
}

// Abuse limits that apply to every owner regardless of plan.
const (
	MaxPerDay     = 600 // uploads started in 24h
	MaxRejectedHr = 15  // rejected uploads in 1h before the owner is blocked
	MaxInFlight   = 2   // concurrent uploads per owner
)
