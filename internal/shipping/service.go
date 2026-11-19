// Package shipping books parcels with a seller's connected courier and keeps
// the order in step. The flow for one order is:
//
//  1. check the order is confirmed and has usable delivery details
//  2. claim the order (unique row) so it can never be booked twice
//  3. call the courier
//  4. record the tracking code, then mark the order shipped
//
// If the courier refuses or cannot be reached, the claim is released and the
// order is left untouched, so the seller can simply try again.
package shipping

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/courier"
	"shopbd/backend/internal/courierconn"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

const (
	CodeNotShippable    = "ORDER_NOT_SHIPPABLE"
	CodeAlreadyShipped  = "ORDER_ALREADY_SHIPPED"
	CodeInFlight        = "SHIPMENT_IN_PROGRESS"
	CodeShipmentInvalid = "SHIPMENT_DETAILS_INVALID"
	CodeShipmentRefused = "SHIPMENT_REJECTED"
	CodeCourierRejected = "COURIER_CREDENTIALS_REJECTED"
	CodeCourierDown     = "COURIER_UNAVAILABLE"

	// MaxBulk keeps a bulk request inside the server's 30s write timeout:
	// 15 orders / 5 workers = 3 rounds of at most shipTimeout each.
	MaxBulk     = 15
	bulkWorkers = 5
	shipTimeout = 8 * time.Second
	// A "pending" claim older than this belongs to a request that died.
	staleClaim = 2 * time.Minute

	statusConfirmed = "confirmed"
)

// OrderInfo is the slice of an order that shipping needs.
type OrderInfo struct {
	ID, Number, Status string
	Name, Phone        string
	Address            string
	Note               string
	COD                float64 // cash to collect; 0 when already paid
}

// Orders is the narrow view of the order store used here.
type Orders interface {
	// Get returns repository.ErrNotFound if the order is not this seller's.
	Get(ctx context.Context, sellerID, orderID string) (*OrderInfo, error)
	// MarkShipped moves a confirmed order to shipped (and records the event).
	MarkShipped(ctx context.Context, sellerID, orderID, note string) error
}

type Connections interface {
	Usable(ctx context.Context, sellerID, id string) (*courierconn.Usable, error)
	ReportRejected(ctx context.Context, sellerID, id string)
}

type Service struct {
	repo   repository.ShipmentRepository
	orders Orders
	conns  Connections
	log    *slog.Logger
	now    func() time.Time
	newID  func() string
}

func New(repo repository.ShipmentRepository, orders Orders, conns Connections, newID func() string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, orders: orders, conns: conns, log: log, newID: newID,
		now: func() time.Time { return time.Now().UTC() }}
}

/* ---------------- views ---------------- */

type View struct {
	ID           string `json:"id"`
	OrderID      string `json:"orderId"`
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	TrackingCode string `json:"trackingCode"`
	TrackingURL  string `json:"trackingUrl,omitempty"`
	CreatedAt    string `json:"createdAt"`
	// Warning is set when the parcel was booked but a follow-up step failed.
	Warning string `json:"warning,omitempty"`
}

type Result struct {
	OrderID  string `json:"orderId"`
	OK       bool   `json:"ok"`
	Shipment *View  `json:"shipment,omitempty"`
	Error    string `json:"error,omitempty"`
}

type failure struct {
	status    int
	code, msg string
}

func (f *failure) Error() string { return f.msg }

func fail(status int, code, msg string) *failure { return &failure{status, code, msg} }

func (s *Service) view(sh *models.Shipment, providerName string) View {
	if providerName == "" {
		providerName = sh.Provider
	}
	return View{ID: sh.ID, OrderID: sh.OrderID, Provider: sh.Provider, ProviderName: providerName,
		TrackingCode: sh.TrackingCode, TrackingURL: sh.TrackingURL, CreatedAt: sh.CreatedAt.UTC().Format(time.RFC3339)}
}

/* ---------------- queries ---------------- */

// List returns the seller's most recent booked shipments (the orders page
// merges these onto its rows).
func (s *Service) List(ctx context.Context, sellerID string, names func(provider string) string) ([]View, error) {
	items, err := s.repo.ListBooked(ctx, sellerID, 500)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(items))
	for _, it := range items {
		out = append(out, s.view(it, names(it.Provider)))
	}
	return out, nil
}

/* ---------------- commands ---------------- */

// Ship books one order. Errors are *apperr.Error-compatible via Convert.
func (s *Service) Ship(ctx context.Context, sellerID, orderID, connectionID string) (*View, error) {
	v, err := s.ship(ctx, sellerID, orderID, connectionID)
	if err != nil {
		return nil, Convert(err)
	}
	return v, nil
}

// ShipMany books several orders with a small worker pool. One order failing
// never stops the others; each gets its own result, in request order.
func (s *Service) ShipMany(ctx context.Context, sellerID string, orderIDs []string, connectionID string) ([]Result, error) {
	seen := map[string]bool{}
	ids := make([]string, 0, len(orderIDs))
	for _, id := range orderIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, apperr.Validation(map[string]string{"orderIds": "Select at least one order"})
	}
	if len(ids) > MaxBulk {
		return nil, apperr.Validation(map[string]string{"orderIds": "You can ship up to 15 orders at once"})
	}
	// Fail the whole request early if the courier itself is unusable.
	if _, err := s.conns.Usable(ctx, sellerID, connectionID); err != nil {
		return nil, err
	}

	results := make([]Result, len(ids))
	sem := make(chan struct{}, bulkWorkers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := s.ship(ctx, sellerID, id, connectionID)
			if err != nil {
				results[i] = Result{OrderID: id, Error: messageOf(err)}
				return
			}
			results[i] = Result{OrderID: id, OK: true, Shipment: v}
		}(i, id)
	}
	wg.Wait()
	return results, nil
}

func (s *Service) ship(ctx context.Context, sellerID, orderID, connectionID string) (*View, error) {
	order, err := s.orders.Get(ctx, sellerID, orderID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}

	// Already shipped? Say so with the existing tracking code.
	if existing, err := s.repo.FindByOrder(ctx, sellerID, orderID); err == nil && existing.State == models.ShipmentBooked {
		return nil, fail(http.StatusConflict, CodeAlreadyShipped, "This order is already shipped. Tracking: "+existing.TrackingCode)
	}
	if order.Status != statusConfirmed {
		msg := "Only confirmed orders can be shipped."
		if order.Status == "pending" {
			msg = "Confirm the order before shipping it."
		}
		return nil, fail(http.StatusUnprocessableEntity, CodeNotShippable, msg)
	}

	req, err := buildRequest(order)
	if err != nil {
		return nil, err
	}

	conn, err := s.conns.Usable(ctx, sellerID, connectionID)
	if err != nil {
		return nil, err
	}

	claim, err := s.claim(ctx, sellerID, order, conn)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, shipTimeout)
	res, err := conn.Shipper.CreateShipment(cctx, conn.Creds, *req)
	cancel()
	if err != nil {
		s.release(ctx, sellerID, claim.ID)
		name := conn.Provider.Name()
		if rej, ok := courier.IsShipmentRejected(err); ok {
			s.log.Info("shipment rejected", "seller_id", sellerID, "order_id", orderID, "provider", conn.Provider.ID())
			return nil, fail(http.StatusUnprocessableEntity, CodeShipmentRefused, name+" refused this parcel: "+rej.Message)
		}
		if errors.Is(err, courier.ErrInvalidCredentials) {
			s.conns.ReportRejected(ctx, sellerID, connectionID)
			return nil, fail(http.StatusUnprocessableEntity, CodeCourierRejected, name+" rejected your saved credentials. Update them on the Courier Services page.")
		}
		s.log.Warn("shipment unconfirmed", "seller_id", sellerID, "order_id", orderID, "provider", conn.Provider.ID())
		return nil, fail(http.StatusBadGateway, CodeCourierDown,
			"Could not confirm the parcel with "+name+". Check your "+name+" dashboard before trying again.")
	}

	// From here the parcel exists at the courier. Never lose that fact because
	// the browser went away: use a context that ignores cancellation.
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bgCancel()

	now := s.now()
	claim.State, claim.ConsignmentID, claim.TrackingCode, claim.TrackingURL, claim.UpdatedAt =
		models.ShipmentBooked, res.ConsignmentID, res.TrackingCode, res.TrackingURL, now
	if err := s.repo.MarkBooked(bg, sellerID, claim.ID, res.ConsignmentID, res.TrackingCode, res.TrackingURL, now); err != nil {
		// The courier has it and the claim row blocks a second booking; log the
		// tracking code so support can recover it.
		s.log.Error("shipment booked but not saved", "seller_id", sellerID, "order_id", orderID,
			"provider", conn.Provider.ID(), "tracking_code", res.TrackingCode, "error", err.Error())
	}
	out := s.view(claim, conn.Provider.Name())
	if err := s.orders.MarkShipped(bg, sellerID, orderID, "Shipped with "+conn.Provider.Name()+". Tracking "+res.TrackingCode); err != nil {
		s.log.Error("shipment booked but order not updated", "seller_id", sellerID, "order_id", orderID, "error", err.Error())
		out.Warning = "The parcel was booked, but the order status could not be updated. Mark it as shipped manually."
	}
	s.log.Info("shipment created", "seller_id", sellerID, "order_id", orderID, "provider", conn.Provider.ID())
	return &out, nil
}

// claim inserts the pending row that reserves the order. A stale pending row
// (a request that died) is cleared once; a fresh one means another request is
// still running.
func (s *Service) claim(ctx context.Context, sellerID string, o *OrderInfo, c *courierconn.Usable) (*models.Shipment, error) {
	now := s.now()
	row := &models.Shipment{ID: s.newID(), SellerID: sellerID, OrderID: o.ID, OrderNumber: o.Number,
		ConnectionID: c.ID, Provider: c.Provider.ID(), State: models.ShipmentPending, CreatedAt: now, UpdatedAt: now}
	for attempt := 0; attempt < 2; attempt++ {
		err := s.repo.Claim(ctx, row)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, repository.ErrShipmentExists) {
			return nil, err
		}
		ex, ferr := s.repo.FindByOrder(ctx, sellerID, o.ID)
		if ferr != nil {
			return nil, ferr
		}
		if ex.State == models.ShipmentBooked {
			return nil, fail(http.StatusConflict, CodeAlreadyShipped, "This order is already shipped. Tracking: "+ex.TrackingCode)
		}
		if now.Sub(ex.CreatedAt) < staleClaim {
			return nil, fail(http.StatusConflict, CodeInFlight, "This order is already being shipped. Wait a moment and refresh.")
		}
		s.release(ctx, sellerID, ex.ID)
	}
	return nil, fail(http.StatusConflict, CodeInFlight, "This order is already being shipped. Wait a moment and refresh.")
}

func (s *Service) release(ctx context.Context, sellerID, id string) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.repo.Release(bg, sellerID, id); err != nil {
		s.log.Error("could not release shipment claim", "seller_id", sellerID, "shipment_id", id, "error", err.Error())
	}
}

/* ---------------- validation ---------------- */

var nonDigit = regexp.MustCompile(`\D`)

// NormalizePhone turns +8801712345678 / 8801712345678 / 01712-345678 into
// 01712345678, or returns "" if it is not a Bangladesh mobile number.
func NormalizePhone(raw string) string {
	d := nonDigit.ReplaceAllString(raw, "")
	switch {
	case strings.HasPrefix(d, "880") && len(d) == 13:
		d = "0" + d[3:]
	case len(d) == 10 && strings.HasPrefix(d, "1"):
		d = "0" + d
	}
	if len(d) == 11 && strings.HasPrefix(d, "01") {
		return d
	}
	return ""
}

var invoiceUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func buildRequest(o *OrderInfo) (*courier.ShipmentRequest, error) {
	phone := NormalizePhone(o.Phone)
	addr := strings.TrimSpace(o.Address)
	name := strings.TrimSpace(o.Name)
	switch {
	case phone == "":
		return nil, fail(http.StatusUnprocessableEntity, CodeShipmentInvalid, "Order "+o.Number+" has a customer phone number the courier cannot use. Fix it and try again.")
	case addr == "":
		return nil, fail(http.StatusUnprocessableEntity, CodeShipmentInvalid, "Order "+o.Number+" has no delivery address.")
	case name == "":
		return nil, fail(http.StatusUnprocessableEntity, CodeShipmentInvalid, "Order "+o.Number+" has no customer name.")
	}
	if r := []rune(addr); len(r) > 250 {
		addr = string(r[:250])
	}
	note := strings.TrimSpace(o.Note)
	if r := []rune(note); len(r) > 200 {
		note = string(r[:200])
	}
	invoice := invoiceUnsafe.ReplaceAllString(o.Number, "-")
	return &courier.ShipmentRequest{Invoice: invoice, Name: name, Phone: phone, Address: addr, CODAmount: o.COD, Note: note}, nil
}

/* ---------------- errors ---------------- */

// Convert turns internal failures into API errors.
func Convert(err error) error {
	var f *failure
	if errors.As(err, &f) {
		return apperr.New(f.status, f.code, f.msg)
	}
	return err
}

func messageOf(err error) string {
	var f *failure
	if errors.As(err, &f) {
		return f.msg
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return "Could not ship this order."
}
