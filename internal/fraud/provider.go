package fraud

import (
	"context"
	"errors"
)

// ProviderResult is a provider's answer in one shape, whatever the provider.
type ProviderResult struct {
	Provider string
	Total    int
	Success  int
	Cancel   int
	// Rating-only signals (Pathao). Rating answers carry no counts, so they
	// are kept apart and never mixed into Total/Success/Cancel.
	Rating    string
	RiskLevel string
	// Couriers is the per-courier breakdown for display.
	Couriers []CourierLine
}

type CourierLine struct {
	Name      string
	Rating    string // set for rating-based answers
	RiskLevel string
	Total     int
	Success   int
	Cancel    int
}

// Provider looks a phone number up with a third party.
type Provider interface {
	ID() string
	Check(ctx context.Context, phone string) (*ProviderResult, error)
}

var (
	// ErrRejected: the provider refused our key (401/403).
	ErrRejected = errors.New("fraud provider rejected the API key")
	// ErrQuota: plan or rate limit reached (429, or "usage limit" message).
	ErrQuota = errors.New("fraud provider limit reached")
	// ErrUnavailable: network failure, 5xx or an unreadable answer.
	ErrUnavailable = errors.New("fraud provider unavailable")
)
