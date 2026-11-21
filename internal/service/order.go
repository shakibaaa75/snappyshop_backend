package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/delivery"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type OrderService struct {
	orders   repository.OrderRepository
	products repository.ProductRepository
	designs  repository.StoreDesignRepository
	mailer   mailer.Mailer

	// quoter prices delivery. nil = the old rule (flat 60 BDT when the store
	// has a "free over X" amount), which is what tests and any deployment
	// without Delivery Settings get.
	quoter DeliveryQuoter

	// How long an unpaid online-payment order keeps its stock reserved before
	// it is cancelled and the stock released. 0 = never expire.
	paymentWindow time.Duration

	// Brand prefix of human-readable order numbers (SNAPPY-20261115-000123).
	numberPrefix string

	// New-order alerts for the seller (see SetSellerAlerts). Optional.
	users       repository.UserRepository
	notifs      repository.NotificationRepository
	frontendURL string

	// Fraud history (see SetOrderHistory). nil = not recording.
	history OrderHistory
}

func NewOrderService(
	orders repository.OrderRepository,
	products repository.ProductRepository,
	designs repository.StoreDesignRepository,
) *OrderService {
	return &OrderService{
		orders:   orders,
		products: products,
		designs:  designs,
		mailer:   mailer.Noop{}, // replaced by SetMailer at startup

		paymentWindow: DefaultPaymentWindow,
		numberPrefix:  DefaultOrderNumberPrefix,
	}
}

// DefaultOrderNumberPrefix starts every new order number.
const DefaultOrderNumberPrefix = "SNAPPY"

// maxOrderAttempts is how many times Create tries to store an order whose ID
// or number turns out to be taken before giving up. A retry gets a new UUIDv7
// and a new sequence number, and is safe because a failed Create reserves no
// stock (see repository.OrderRepository.Create).
const maxOrderAttempts = 5

// orderNumberZone is Bangladesh time (UTC+6, no daylight saving), so the date
// inside an order number is the shop's calendar day, not UTC's. A fixed zone
// avoids depending on tzdata being installed in the container.
var orderNumberZone = time.FixedZone("BST", 6*60*60)

// SetOrderNumberPrefix changes the brand prefix of new order numbers. Only
// letters and digits are kept, upper-cased, up to 12 characters; an empty
// result keeps the current prefix.
func (s *OrderService) SetOrderNumberPrefix(p string) {
	var b strings.Builder
	for _, r := range strings.ToUpper(p) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == 12 {
			break
		}
	}
	if b.Len() > 0 {
		s.numberPrefix = b.String()
	}
}

// DefaultPaymentWindow is how long an order that must be paid online
// (bKash / Nagad / Rocket / card) holds its stock while unpaid.
const DefaultPaymentWindow = 24 * time.Hour

// SetPaymentWindow changes the payment window. Pass 0 to disable automatic
// expiry (stock then stays reserved until the seller cancels the order).
func (s *OrderService) SetPaymentWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.paymentWindow = d
}

// SetMailer enables order emails. Call it once at startup, right after
// NewOrderService.
func (s *OrderService) SetMailer(m mailer.Mailer) {
	if m != nil {
		s.mailer = m
	}
}

// DeliveryQuoter prices the delivery of one seller's order. DeliveryService
// is the real implementation; the order service only needs this one method.
type DeliveryQuoter interface {
	Quote(ctx context.Context, sellerID, district string, productIDs []string, subtotal float64) (delivery.Breakdown, error)
}

// SetDeliveryQuoter makes orders use the seller's Delivery Settings. Call it
// once at startup. Without it orders use the old flat-fee rule.
func (s *OrderService) SetDeliveryQuoter(q DeliveryQuoter) {
	s.quoter = q
}

/* ------------------------------------------------------------------ */
/* Input / output types                                                */
/* ------------------------------------------------------------------ */

type OrderLineInput struct {
	ProductID string
	VariantID string
	Qty       int
}

type CreateOrderInput struct {
	BuyerID         string
	CustomerName    string
	CustomerPhone   string
	CustomerEmail   string
	ShippingAddress string
	ShippingCity    string
	ShippingArea    string
	Notes           string
	PaymentMethod   string
	Lines           []OrderLineInput
}

/* ------------------------------------------------------------------ */
/* Public: create order                                                */
/* ------------------------------------------------------------------ */

// CreateBySlug is called by the public checkout page. It resolves the store
// slug to a seller, validates every line against the current product data,
// computes the totals server-side, and inserts the order.
func (s *OrderService) CreateBySlug(ctx context.Context, slug string, in CreateOrderInput) (*models.Order, error) {
	sellerID, err := s.sellerIDBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if sellerID == "" {
		return nil, apperr.NotFound()
	}
	return s.Create(ctx, sellerID, in)
}

// Create validates and stores an order for a seller.
func (s *OrderService) Create(ctx context.Context, sellerID string, in CreateOrderInput) (*models.Order, error) {
	if err := validateOrderInput(in); err != nil {
		return nil, err
	}

	// Validate products and build lines with server-side pricing.
	lines, subtotal, err := s.priceLines(ctx, sellerID, in.Lines, true)
	if err != nil {
		return nil, err
	}

	// Delivery is priced by the same code that answers the checkout quote, so
	// what the customer was shown is what they are charged.
	bd, err := s.quoteDelivery(ctx, sellerID, in.ShippingCity, lines, subtotal)
	if err != nil {
		return nil, err
	}
	const discount = 0.0 // no discounts exist yet; the field is stored for later
	total := orderTotal(subtotal, bd.Charge, discount)

	now := time.Now().UTC()
	order := &models.Order{
		SellerID:        sellerID,
		BuyerID:         in.BuyerID,
		CustomerName:    strings.TrimSpace(in.CustomerName),
		CustomerPhone:   strings.TrimSpace(in.CustomerPhone),
		CustomerEmail:   strings.TrimSpace(in.CustomerEmail),
		ShippingAddress: strings.TrimSpace(in.ShippingAddress),
		ShippingCity:    strings.TrimSpace(in.ShippingCity),
		ShippingArea:    strings.TrimSpace(in.ShippingArea),
		Notes:           strings.TrimSpace(in.Notes),
		PaymentMethod:   models.PaymentMethod(in.PaymentMethod),
		Paid:            false,
		Subtotal:        subtotal,
		Delivery:        bd.Charge,
		Discount:        discount,
		Total:           total,
		DeliveryZone:    bd.Zone,
		DeliveryBase:    bd.Base,
		DeliveryWaived:  bd.Waived,
		Lines:           lines,
		Status:          models.OrderPending,
		Events: []models.OrderStatusEvent{
			{Status: models.OrderPending, At: now, Note: "Order placed by customer"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Online-payment orders hold their stock only for the payment window.
	if order.PaymentMethod != models.PayCOD && s.paymentWindow > 0 {
		exp := now.Add(s.paymentWindow)
		order.ReservationExpiresAt = &exp
	}

	// Give the order its unique ID and readable number, then store it. If the
	// ID or number is somehow already taken (two instances, a restored
	// backup, a counter that was reset), try again with fresh ones.
	// orders.Create reserves the stock for every line and stores the order, or
	// does neither, so a retry can never double-reserve.
	for attempt := 1; ; attempt++ {
		number, err := s.nextOrderNumber(ctx, now)
		if err != nil {
			return nil, err
		}
		order.ID = models.NewUUIDv7()
		order.Number = number

		err = s.orders.Create(ctx, order)
		if err == nil {
			break
		}
		if errors.Is(err, repository.ErrDuplicateOrder) {
			if attempt >= maxOrderAttempts {
				return nil, fmt.Errorf("order: no unique id/number after %d attempts: %w", attempt, err)
			}
			slog.Warn("order id/number collision, retrying", "number", number, "attempt", attempt)
			continue
		}
		var ise *repository.InsufficientStockError
		if errors.As(err, &ise) {
			return nil, apperr.Validation(map[string]string{
				"lines": stockMessage(ise.Name, ise.Available),
			})
		}
		return nil, err
	}

	// Tell the seller (bell + email) in the background; copy the order so the
	// goroutine does not share the pointer returned to the caller.
	go s.alertSellerNewOrder(*order)

	return order, nil
}

/* ------------------------------------------------------------------ */
/* Seller: list / get / update                                         */
/* ------------------------------------------------------------------ */

func (s *OrderService) List(ctx context.Context, sellerID string, status string, limit int) ([]*models.Order, error) {
	filter := repository.OrderFilter{Limit: limit}
	if status != "" && status != "all" {
		st := models.OrderStatus(status)
		filter.Status = &st
	}
	return s.orders.List(ctx, sellerID, filter)
}

func (s *OrderService) Get(ctx context.Context, sellerID, id string) (*models.Order, error) {
	o, err := s.orders.FindByID(ctx, sellerID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	return o, nil
}

// UpdateStatus moves an order to a new status. When the status actually
// changes to confirmed, shipped or delivered, the customer is emailed in the
// background (if they gave an email address).
func (s *OrderService) UpdateStatus(ctx context.Context, sellerID, id, status, note string) (*models.Order, error) {
	st := models.OrderStatus(status)
	switch st {
	case models.OrderPending, models.OrderConfirmed, models.OrderShipped, models.OrderDelivered, models.OrderCancelled:
	default:
		return nil, apperr.Validation(map[string]string{"status": "Invalid status"})
	}

	// Load the current order first so we only email on a real change
	// (clicking the same button twice must not send a second email).
	before, err := s.Get(ctx, sellerID, id)
	if err != nil {
		return nil, err
	}

	if err := s.orders.UpdateStatus(ctx, sellerID, id, st, strings.TrimSpace(note), time.Now().UTC()); err != nil {
		var ise *repository.InsufficientStockError
		switch {
		case errors.Is(err, repository.ErrNotFound):
			return nil, apperr.NotFound()
		case errors.Is(err, repository.ErrOrderChanged):
			return nil, apperr.Conflict("ORDER_CHANGED", "This order was just updated. Please refresh and try again.", "")
		case errors.As(err, &ise):
			// Re-opening a cancelled order needs its stock back.
			return nil, apperr.Validation(map[string]string{
				"status": "Can't reopen this order: " + stockMessage(ise.Name, ise.Available),
			})
		}
		return nil, err
	}

	updated, err := s.Get(ctx, sellerID, id)
	if err != nil {
		return nil, err
	}

	if before.Status != st {
		go s.notifyStatus(*updated) // copy: the goroutine must not share the pointer
		s.recordHistory(*updated)
	}
	return updated, nil
}

func (s *OrderService) MarkPaid(ctx context.Context, sellerID, id string, paid bool) (*models.Order, error) {
	if err := s.orders.MarkPaid(ctx, sellerID, id, paid); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	return s.Get(ctx, sellerID, id)
}

// RunReservationSweeper cancels unpaid online-payment orders whose payment
// window has passed and releases their stock, until ctx is cancelled. Start it
// once at boot with `go svc.RunReservationSweeper(ctx, time.Minute)`. It is
// safe to run on several instances at once.
func (s *OrderService) RunReservationSweeper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *OrderService) sweepOnce(ctx context.Context) {
	const batch = 100
	for {
		expired, err := s.orders.ExpireUnpaid(ctx, time.Now().UTC(), batch)
		for _, o := range expired {
			slog.Info("unpaid order expired, stock released", "order", o.Number, "seller", o.SellerID)
			go s.notifyStatus(*o)
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("order expiry sweep failed", "err", err)
			}
			return
		}
		if len(expired) < batch {
			return
		}
	}
}

func (s *OrderService) Stats(ctx context.Context, sellerID string, since time.Time) (*repository.OrderStats, error) {
	return s.orders.Stats(ctx, sellerID, since)
}

/* ------------------------------------------------------------------ */
/* Helpers                                                             */
/* ------------------------------------------------------------------ */

func (s *OrderService) sellerIDBySlug(ctx context.Context, slug string) (string, error) {
	d, err := s.designs.GetPublishedBySlug(ctx, strings.ToLower(slug))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return d.SellerID, nil
}

// priceLines checks every requested line against the stored product and
// returns the order lines (priced from the product, never from the client)
// and their subtotal. With checkStock the early stock check is applied too;
// a quote leaves it off because the cart may still be edited, and the atomic
// reservation in orders.Create is the real guard anyway.
func (s *OrderService) priceLines(ctx context.Context, sellerID string, in []OrderLineInput, checkStock bool) ([]models.OrderLine, float64, error) {
	lines := make([]models.OrderLine, 0, len(in))
	subtotal := 0.0
	for _, l := range in {
		p, err := s.products.FindByID(ctx, sellerID, l.ProductID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return nil, 0, apperr.Validation(map[string]string{
					"lines": fmt.Sprintf("Product %s is no longer available", l.ProductID),
				})
			}
			return nil, 0, err
		}
		if !p.IsActive {
			return nil, 0, apperr.Validation(map[string]string{
				"lines": fmt.Sprintf("%s is not currently for sale", p.Name),
			})
		}

		// What this line buys comes only from the stored product: a product
		// with variants is sold as one of its own variants, at that variant's
		// own price and from that variant's own stock. Nothing falls back to
		// the product's base price.
		sel, problem := resolveVariant(p, l.VariantID)
		if problem != "" {
			return nil, 0, apperr.Validation(map[string]string{"lines": problem})
		}
		price, available := sel.Price, sel.Stock

		// Friendly early check. It is NOT what prevents overselling: the
		// authoritative, atomic check happens in orders.Create.
		if checkStock && l.Qty > available {
			return nil, 0, apperr.Validation(map[string]string{
				"lines": stockMessage(p.Name, available),
			})
		}

		lineSubtotal := price * float64(l.Qty)
		subtotal += lineSubtotal

		lines = append(lines, models.OrderLine{
			ProductID: p.ID,
			Name:      p.Name,
			ImageURL:  p.MainImageURL,
			Emoji:     p.Emoji,
			Hue:       p.Hue,
			VariantID: sel.ID,
			Variant:   sel.Label,
			Price:     price,
			Qty:       l.Qty,
			Subtotal:  lineSubtotal,
		})
	}
	return lines, subtotal, nil
}

// quoteDelivery prices the delivery of an order that has these lines and is
// going to this district. The district is the shipping city the customer
// picked at checkout.
func (s *OrderService) quoteDelivery(ctx context.Context, sellerID, district string, lines []models.OrderLine, subtotal float64) (delivery.Breakdown, error) {
	ids := make([]string, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.ProductID)
	}
	if s.quoter != nil {
		return s.quoter.Quote(ctx, sellerID, district, ids, subtotal)
	}
	// No Delivery Settings wired in: the old rule, expressed as legacy
	// settings so there is only one pricing function.
	threshold := 0.0
	if d, err := s.designs.GetBySeller(ctx, sellerID); err == nil && d != nil {
		threshold = extractFreeDeliveryThreshold(d.Draft)
	}
	return delivery.Calculate(delivery.Legacy(threshold), delivery.ZoneFor(district), ids, subtotal, threshold), nil
}

// orderTotal is what the customer pays: subtotal plus delivery minus any
// discount, rounded to 2 decimals and never below 0.
func orderTotal(subtotal, deliveryCharge, discount float64) float64 {
	t := delivery.Round(subtotal + deliveryCharge - discount)
	if t < 0 {
		return 0
	}
	return t
}

// DeliveryQuote is the price of an order before it is placed: what checkout
// shows live as the customer picks a district.
type DeliveryQuote struct {
	Zone     models.DeliveryZone
	Subtotal float64
	// Delivery is what the customer pays for delivery. DeliveryBase is the
	// charge before the free-delivery waiver; FreeDeliveryApplied is true when
	// the waiver took it to 0.
	Delivery            float64
	DeliveryBase        float64
	FreeDeliveryApplied bool
	Discount            float64
	Total               float64
}

// QuoteInput is what a quote needs: the destination district and the cart.
type QuoteInput struct {
	ShippingCity string
	Lines        []OrderLineInput
}

// QuoteBySlug prices a cart for a published store without placing an order.
// It runs the same line pricing and delivery rules as Create, so the quote and
// the final order cannot disagree. It does not check stock.
func (s *OrderService) QuoteBySlug(ctx context.Context, slug string, in QuoteInput) (*DeliveryQuote, error) {
	sellerID, err := s.sellerIDBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if sellerID == "" {
		return nil, apperr.NotFound()
	}
	if err := validateOrderLines(in.Lines); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(in.ShippingCity)) < 2 {
		return nil, apperr.Validation(map[string]string{"shippingCity": "Please choose the district"})
	}

	lines, subtotal, err := s.priceLines(ctx, sellerID, in.Lines, false)
	if err != nil {
		return nil, err
	}
	bd, err := s.quoteDelivery(ctx, sellerID, in.ShippingCity, lines, subtotal)
	if err != nil {
		return nil, err
	}
	const discount = 0.0
	return &DeliveryQuote{
		Zone:                bd.Zone,
		Subtotal:            delivery.Round(subtotal),
		Delivery:            bd.Charge,
		DeliveryBase:        bd.Base,
		FreeDeliveryApplied: bd.Waived,
		Discount:            discount,
		Total:               orderTotal(subtotal, bd.Charge, discount),
	}, nil
}

func extractFreeDeliveryThreshold(raw any) float64 {
	m, ok := raw.(map[string]any)
	if !ok {
		return 0
	}
	v, ok := m["freeDeliveryThreshold"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func stockMessage(name string, available int) string {
	if available <= 0 {
		return fmt.Sprintf("%s is out of stock", name)
	}
	return fmt.Sprintf("%s only has %d left in stock", name, available)
}

// nextOrderNumber takes the next value of today's database sequence and
// formats it, e.g. SNAPPY-20261115-000123. The sequence restarts at 1 each
// shop-local day (one counter document per day) and is issued by MongoDB, so
// two orders can never be given the same number, whichever instance serves them.
func (s *OrderService) nextOrderNumber(ctx context.Context, now time.Time) (string, error) {
	day := now.In(orderNumberZone).Format("20060102")
	seq, err := s.orders.NextOrderSequence(ctx, "order-number:"+day)
	if err != nil {
		return "", fmt.Errorf("order number sequence: %w", err)
	}
	return formatOrderNumber(s.numberPrefix, now, seq), nil
}

// formatOrderNumber builds PREFIX-YYYYMMDD-NNNNNN (6+ digits, never truncated).
func formatOrderNumber(prefix string, now time.Time, seq int64) string {
	return fmt.Sprintf("%s-%s-%06d", prefix, now.In(orderNumberZone).Format("20060102"), seq)
}

func validateOrderInput(in CreateOrderInput) error {
	fields := map[string]string{}

	name := strings.TrimSpace(in.CustomerName)
	if len(name) < 2 {
		fields["customerName"] = "Please enter the customer's name"
	}

	phone := strings.ReplaceAll(strings.TrimSpace(in.CustomerPhone), " ", "")
	phone = strings.ReplaceAll(phone, "-", "")
	if len(phone) < 11 {
		fields["customerPhone"] = "Please enter a valid phone number"
	}

	// Email is optional, but if given it must be a plain address
	// (order updates are sent to it).
	if email := strings.TrimSpace(in.CustomerEmail); email != "" {
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email {
			fields["customerEmail"] = "Please enter a valid email address"
		}
	}

	if len(strings.TrimSpace(in.ShippingAddress)) < 4 {
		fields["shippingAddress"] = "Please enter the delivery address"
	}
	if len(strings.TrimSpace(in.ShippingCity)) < 2 {
		fields["shippingCity"] = "Please enter the city"
	}

	switch models.PaymentMethod(in.PaymentMethod) {
	case models.PayBkash, models.PayNagad, models.PayRocket, models.PayCOD, models.PayCard:
	default:
		fields["paymentMethod"] = "Invalid payment method"
	}

	if msg := orderLinesProblem(in.Lines); msg != "" {
		fields["lines"] = msg
	}

	if len(fields) > 0 {
		return apperr.Validation(fields)
	}
	return nil
}

// orderLinesProblem returns what is wrong with the requested lines, or "".
func orderLinesProblem(lines []OrderLineInput) string {
	msg := ""
	if len(lines) == 0 {
		msg = "Your cart is empty"
	}
	if len(lines) > 100 {
		msg = "Too many items"
	}
	for _, l := range lines {
		if l.Qty <= 0 {
			return "Quantity must be at least 1"
		}
		if l.Qty > 999 {
			return "Quantity is too high"
		}
	}
	return msg
}

// validateOrderLines is orderLinesProblem as an API error.
func validateOrderLines(lines []OrderLineInput) error {
	if msg := orderLinesProblem(lines); msg != "" {
		return apperr.Validation(map[string]string{"lines": msg})
	}
	return nil
}
