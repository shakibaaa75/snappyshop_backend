package repository

import (
	"context"
	"errors"
	"time"

	"shopbd/backend/internal/models"
)

// ErrShipmentExists means this order already has a shipment row.
var ErrShipmentExists = errors.New("repository: shipment already exists for order")

// ShipmentRepository stores parcels. Every method is scoped by sellerID.
type ShipmentRepository interface {
	// Claim inserts a pending shipment. ErrShipmentExists if the order has one.
	Claim(ctx context.Context, s *models.Shipment) error
	FindByOrder(ctx context.Context, sellerID, orderID string) (*models.Shipment, error)
	// MarkBooked records the courier's answer on a pending shipment.
	MarkBooked(ctx context.Context, sellerID, id string, consignmentID, trackingCode, trackingURL string, at time.Time) error
	// Release deletes a shipment only if it is still pending.
	Release(ctx context.Context, sellerID, id string) error
	// ListBooked returns the seller's most recent booked shipments.
	ListBooked(ctx context.Context, sellerID string, limit int) ([]*models.Shipment, error)
}
