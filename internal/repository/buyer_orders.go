package repository

import (
	"context"
	"errors"
	"time"

	"shopbd/backend/internal/models"
)

// ErrIssueOpen means the order already has an open problem report.
var ErrIssueOpen = errors.New("repository: order already has an open issue")

// BuyerOrderRepository reads orders across every seller by the buyer's
// verified email address (compared without regard to upper/lower case).
type BuyerOrderRepository interface {
	// ListByEmail returns newest first; before (when set) returns only older orders.
	ListByEmail(ctx context.Context, email string, limit int, before time.Time) ([]*models.Order, error)
	// FindByEmailAndID returns ErrNotFound unless the order belongs to this email.
	FindByEmailAndID(ctx context.Context, email, orderID string) (*models.Order, error)
}

// OrderIssueRepository stores "Problem with this order" reports.
type OrderIssueRepository interface {
	// Create returns ErrIssueOpen when the order already has an open issue.
	Create(ctx context.Context, i *models.OrderIssue) error
	ListByBuyer(ctx context.Context, buyerID string, limit int) ([]*models.OrderIssue, error)
	// Withdraw closes the buyer's own open issue; ErrNotFound if it is not theirs or not open.
	Withdraw(ctx context.Context, buyerID, issueID string, at time.Time) error
}
