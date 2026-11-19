package repository

import (
	"context"
	"time"

	"shopbd/backend/internal/models"
)

// NotificationRepository stores admin-to-seller notifications and who read them.
type NotificationRepository interface {
	Create(ctx context.Context, n *models.Notification) error
	// Delete removes the notification and every read mark for it.
	// Returns ErrNotFound when it does not exist.
	Delete(ctx context.Context, id string) error

	// ListAdmin returns every notification, newest first, plus the total.
	ListAdmin(ctx context.Context, limit, skip int) ([]*models.Notification, int64, error)
	// ReadCounts returns how many users read each notification.
	ReadCounts(ctx context.Context, ids []string) (map[string]int64, error)

	// ListForSeller returns the notifications this seller may see: sent to
	// everyone or to this seller, created at or after `since`, and not expired
	// at `now`. Newest first.
	ListForSeller(ctx context.Context, sellerID string, since, now time.Time, limit int) ([]*models.Notification, error)
	// ReadSet returns which of ids this user has already read.
	ReadSet(ctx context.Context, userID string, ids []string) (map[string]bool, error)
	// MarkRead marks ids as read for this user. Safe to repeat.
	MarkRead(ctx context.Context, userID string, ids []string, at time.Time) error

	// ExistingSellerIDs returns the subset of ids that belong to sellers.
	ExistingSellerIDs(ctx context.Context, ids []string) ([]string, error)
}
