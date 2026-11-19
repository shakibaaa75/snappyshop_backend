package courier

import (
	"context"
	"errors"
)

// ErrShipmentRejected means the courier understood the request but refused to
// create the parcel (bad phone number, address too long, duplicate invoice...).
// Message is safe to show the seller and never contains credentials.
type ErrShipmentRejected struct{ Message string }

func (e *ErrShipmentRejected) Error() string { return "courier: shipment rejected: " + e.Message }

// IsShipmentRejected unwraps an ErrShipmentRejected.
func IsShipmentRejected(err error) (*ErrShipmentRejected, bool) {
	var r *ErrShipmentRejected
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// ShipmentRequest is everything a courier needs to book one parcel. It is
// provider-neutral; each adapter maps it to its own API.
type ShipmentRequest struct {
	// Invoice is a unique reference for the parcel (we use the order number).
	Invoice   string
	Name      string
	Phone     string // normalised Bangladesh mobile, 01XXXXXXXXX
	Address   string
	CODAmount float64 // amount to collect on delivery; 0 for prepaid orders
	Note      string
}

// ShipmentResult is what the courier returns after booking.
type ShipmentResult struct {
	ConsignmentID string
	TrackingCode  string
	TrackingURL   string
}

// Shipper is implemented by providers that can book parcels. It is separate
// from Provider so a courier can be connectable before booking is built.
//
// CreateShipment returns ErrInvalidCredentials, *ErrShipmentRejected, or
// ErrUnavailable. ErrUnavailable after the request was sent means the outcome
// is unknown; callers must tell the seller to check the courier dashboard.
type Shipper interface {
	CreateShipment(ctx context.Context, creds map[string]string, req ShipmentRequest) (*ShipmentResult, error)
}
