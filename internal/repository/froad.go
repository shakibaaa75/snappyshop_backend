package repository

import (
	"context"
	"time"

	"shopbd/backend/internal/models"
)

// FraudSettingsRepository stores the single platform-wide fraud configuration.
type FraudSettingsRepository interface {
	// Get returns ErrNotFound until the admin saves settings for the first time.
	Get(ctx context.Context) (*models.FraudSettings, error)
	Upsert(ctx context.Context, s *models.FraudSettings) error
}

// FraudDelta is a change to a customer's counters.
type FraudDelta struct {
	Delivered, Cancelled, Failed, Open int
}

// Add changes the counter for the given outcome by n (use a negative n to
// take an order out of that counter).
func (d *FraudDelta) Add(o models.FraudOutcome, n int) {
	switch o {
	case models.FraudDelivered:
		d.Delivered += n
	case models.FraudCancelled:
		d.Cancelled += n
	case models.FraudFailed:
		d.Failed += n
	case models.FraudOpen:
		d.Open += n
	}
}

// FraudHistoryRepository keeps the per-phone counters and the per-order
// events that make those counters idempotent.
type FraudHistoryRepository interface {
	// SwapEvent stores ev as the order's current event unless a newer one is
	// already stored. It returns the previous event (nil if none) and
	// applied=false when ev was stale.
	SwapEvent(ctx context.Context, ev *models.FraudOrderEvent) (prev *models.FraudOrderEvent, applied bool, err error)
	// RemoveEvent deletes the order's event unless it is newer than at.
	// Returns the removed event, or nil if nothing was removed.
	RemoveEvent(ctx context.Context, orderID string, at time.Time) (*models.FraudOrderEvent, error)
	// AdjustCustomer applies d to the phone's counters, creating the record.
	AdjustCustomer(ctx context.Context, phone string, d FraudDelta, at time.Time) error
	// GetCustomer returns ErrNotFound for a phone never seen.
	GetCustomer(ctx context.Context, phone string) (*models.FraudCustomer, error)
	SaveExternal(ctx context.Context, phone string, ext *models.FraudExternal, at time.Time) error
	// DeleteInactiveBefore removes records with no activity since cutoff and
	// returns how many documents were deleted.
	DeleteInactiveBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// FraudCheckStore keeps the last fraud check result of each order.
type FraudCheckStore interface {
	SaveCheck(ctx context.Context, c *models.FraudCheckSnapshot) error
	// GetCheck returns ErrNotFound when this seller has no saved check for the order.
	GetCheck(ctx context.Context, sellerID, orderID string) (*models.FraudCheckSnapshot, error)
}
