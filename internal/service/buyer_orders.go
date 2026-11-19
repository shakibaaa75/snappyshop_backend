package service

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

const (
	buyerOrdersDefaultLimit = 20
	buyerOrdersMaxLimit     = 50
	issueMinMessage         = 10
	issueMaxMessage         = 1000
	buyerIssuesLimit        = 100
)

// issueRules says in which order states each kind of problem can be reported.
var issueRules = map[string]func(o *models.Order) bool{
	models.IssueNotReceived: func(o *models.Order) bool {
		return o.Status == models.OrderShipped || o.Status == models.OrderDelivered
	},
	models.IssueWrongItem:   func(o *models.Order) bool { return o.Status == models.OrderDelivered },
	models.IssueDamaged:     func(o *models.Order) bool { return o.Status == models.OrderDelivered },
	models.IssueMissingItem: func(o *models.Order) bool { return o.Status == models.OrderDelivered },
	models.IssueRefund: func(o *models.Order) bool {
		return o.Status == models.OrderDelivered || (o.Status == models.OrderCancelled && o.Paid)
	},
	models.IssueOther: func(o *models.Order) bool { return true },
}

// issueOrder keeps the list of allowed types in a stable, friendly order.
var issueOrder = []string{
	models.IssueNotReceived, models.IssueWrongItem, models.IssueDamaged,
	models.IssueMissingItem, models.IssueRefund, models.IssueOther,
}

// BuyerOrderService is the buyer's "My orders" across all sellers.
//
// Orders are not tied to a user id (guests can check out), so an order is the
// buyer's when its checkout email equals the buyer's *verified* account email.
// Phone numbers are never used: they are not verified.
type BuyerOrderService struct {
	users     repository.UserRepository
	orders    repository.BuyerOrderRepository
	shipments repository.ShipmentRepository
	stores    repository.StoreDesignRepository
	issues    repository.OrderIssueRepository
	notifs    repository.NotificationRepository
	log       *slog.Logger
	now       func() time.Time
}

func NewBuyerOrderService(
	users repository.UserRepository,
	orders repository.BuyerOrderRepository,
	shipments repository.ShipmentRepository,
	stores repository.StoreDesignRepository,
	issues repository.OrderIssueRepository,
	notifs repository.NotificationRepository,
	log *slog.Logger,
) *BuyerOrderService {
	if log == nil {
		log = slog.Default()
	}
	return &BuyerOrderService{
		users: users, orders: orders, shipments: shipments, stores: stores,
		issues: issues, notifs: notifs, log: log, now: time.Now,
	}
}

// ------------------------------------------------------------------ views

type BuyerSellerRef struct {
	Name string
	Slug string
}

type BuyerOrderLine struct {
	Name     string
	ImageURL string
	Emoji    string
	Hue      int
	Variant  string
	Price    float64
	Qty      int
	Subtotal float64
}

type BuyerOrderSummary struct {
	ID           string
	Number       string
	Status       models.OrderStatus
	CreatedAt    time.Time
	Total        float64
	ItemCount    int
	Preview      []BuyerOrderLine // first 3 lines
	Seller       BuyerSellerRef
	HasOpenIssue bool
}

type BuyerOrderList struct {
	Orders        []BuyerOrderSummary
	NextBefore    *time.Time // pass back as "before" for the next page
	EmailVerified bool
}

// TimelineStep is one stage on the order's tracking timeline.
type TimelineStep struct {
	Key   string // placed | confirmed | shipped | delivered | cancelled
	Label string
	Done  bool
	At    *time.Time
}

type BuyerTracking struct {
	Courier string
	Code    string
	URL     string // empty unless it is a safe https link
}

type BuyerOrderDetail struct {
	BuyerOrderSummary
	Lines    []BuyerOrderLine
	Subtotal float64
	Delivery float64
	Discount float64
	// DeliveryZone is "inside_dhaka" / "outside_dhaka", empty on orders placed
	// before Delivery Settings existed. DeliveryBase is the charge before a
	// free-delivery waiver; DeliveryWaived is true when the waiver applied.
	DeliveryZone   models.DeliveryZone
	DeliveryBase   float64
	DeliveryWaived bool
	PaymentMethod  models.PaymentMethod
	Paid           bool
	CustomerName   string
	Phone          string
	Address        string
	City           string
	Area           string
	Timeline       []TimelineStep
	Tracking       *BuyerTracking
	Issues         []*models.OrderIssue
	ReportTypes    []string // problem kinds the buyer may report right now
}

// ------------------------------------------------------------------ orders

func (s *BuyerOrderService) verifiedEmail(ctx context.Context, userID string) (string, bool, error) {
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", false, apperr.NotFound()
		}
		return "", false, err
	}
	if !u.EmailVerified {
		return "", false, nil
	}
	return strings.TrimSpace(u.Email), true, nil
}

func (s *BuyerOrderService) Orders(ctx context.Context, userID string, limit int, before time.Time) (*BuyerOrderList, error) {
	email, verified, err := s.verifiedEmail(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !verified {
		return &BuyerOrderList{Orders: []BuyerOrderSummary{}, EmailVerified: false}, nil
	}
	if limit <= 0 {
		limit = buyerOrdersDefaultLimit
	}
	if limit > buyerOrdersMaxLimit {
		limit = buyerOrdersMaxLimit
	}
	orders, err := s.orders.ListByEmail(ctx, email, limit, before)
	if err != nil {
		return nil, err
	}
	issues, err := s.issues.ListByBuyer(ctx, userID, buyerIssuesLimit)
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	for _, i := range issues {
		if i.Status == models.IssueOpen {
			open[i.OrderID] = true
		}
	}

	sellers := map[string]BuyerSellerRef{}
	out := &BuyerOrderList{Orders: make([]BuyerOrderSummary, 0, len(orders)), EmailVerified: true}
	for _, o := range orders {
		sum := s.summary(ctx, o, sellers)
		sum.HasOpenIssue = open[o.ID]
		out.Orders = append(out.Orders, sum)
	}
	if len(orders) == limit {
		t := orders[len(orders)-1].CreatedAt
		out.NextBefore = &t
	}
	return out, nil
}

func (s *BuyerOrderService) Order(ctx context.Context, userID, orderID string) (*BuyerOrderDetail, error) {
	email, verified, err := s.verifiedEmail(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, apperr.NotFound()
	}
	o, err := s.orders.FindByEmailAndID(ctx, email, orderID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}

	sellers := map[string]BuyerSellerRef{}
	d := &BuyerOrderDetail{
		BuyerOrderSummary: s.summary(ctx, o, sellers),
		Subtotal:          o.Subtotal,
		Delivery:          o.Delivery,
		Discount:          o.Discount,
		DeliveryZone:      o.DeliveryZone,
		DeliveryBase:      o.DeliveryBase,
		DeliveryWaived:    o.DeliveryWaived,
		PaymentMethod:     o.PaymentMethod,
		Paid:              o.Paid,
		CustomerName:      o.CustomerName,
		Phone:             o.CustomerPhone,
		Address:           o.ShippingAddress,
		City:              o.ShippingCity,
		Area:              o.ShippingArea,
		Lines:             make([]BuyerOrderLine, 0, len(o.Lines)),
	}
	for _, l := range o.Lines {
		d.Lines = append(d.Lines, toBuyerLine(l))
	}

	// Courier details exist only once the seller booked the parcel.
	var booked *models.Shipment
	if sh, err := s.shipments.FindByOrder(ctx, o.SellerID, o.ID); err == nil && sh.State == models.ShipmentBooked {
		booked = sh
		d.Tracking = &BuyerTracking{Courier: sh.Provider, Code: sh.TrackingCode, URL: safeHTTPS(sh.TrackingURL)}
	} else if err != nil && !errors.Is(err, repository.ErrNotFound) {
		s.log.Warn("buyer order: shipment lookup failed", "orderId", o.ID, "error", err.Error())
	}
	d.Timeline = BuildTimeline(o, booked)

	// This buyer's reports on this order, newest first.
	all, err := s.issues.ListByBuyer(ctx, userID, buyerIssuesLimit)
	if err != nil {
		return nil, err
	}
	for _, i := range all {
		if i.OrderID == o.ID {
			d.Issues = append(d.Issues, i)
		}
	}
	d.HasOpenIssue = hasOpen(d.Issues)
	if !d.HasOpenIssue {
		d.ReportTypes = AllowedIssueTypes(o)
	}
	return d, nil
}

func (s *BuyerOrderService) summary(ctx context.Context, o *models.Order, cache map[string]BuyerSellerRef) BuyerOrderSummary {
	ref, ok := cache[o.SellerID]
	if !ok {
		ref = BuyerSellerRef{Name: "Seller"}
		if sd, err := s.stores.GetBySeller(ctx, o.SellerID); err == nil && sd != nil {
			if sd.StoreName != "" {
				ref.Name = sd.StoreName
			}
			ref.Slug = sd.Slug
		}
		cache[o.SellerID] = ref
	}
	sum := BuyerOrderSummary{
		ID: o.ID, Number: o.Number, Status: o.Status, CreatedAt: o.CreatedAt,
		Total: o.Total, Seller: ref,
	}
	for i, l := range o.Lines {
		sum.ItemCount += l.Qty
		if i < 3 {
			sum.Preview = append(sum.Preview, toBuyerLine(l))
		}
	}
	return sum
}

func toBuyerLine(l models.OrderLine) BuyerOrderLine {
	return BuyerOrderLine{
		Name: l.Name, ImageURL: l.ImageURL, Emoji: l.Emoji, Hue: l.Hue,
		Variant: l.Variant, Price: l.Price, Qty: l.Qty, Subtotal: l.Subtotal,
	}
}

func hasOpen(list []*models.OrderIssue) bool {
	for _, i := range list {
		if i.Status == models.IssueOpen {
			return true
		}
	}
	return false
}

// BuildTimeline turns the order's status history (and the booked parcel, if
// any) into the steps the buyer sees. Steps that were never reached are
// returned with Done=false so the UI can show what comes next.
func BuildTimeline(o *models.Order, booked *models.Shipment) []TimelineStep {
	at := map[models.OrderStatus]time.Time{}
	for _, e := range o.Events {
		// Keep the first time each status was reached.
		if _, seen := at[e.Status]; !seen {
			at[e.Status] = e.At
		}
	}
	ptr := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}

	steps := []TimelineStep{{Key: "placed", Label: "Order placed", Done: true, At: ptr(o.CreatedAt)}}

	if o.Status == models.OrderCancelled {
		t := at[models.OrderCancelled]
		if t.IsZero() {
			t = o.UpdatedAt
		}
		// Show the stages reached before the cancellation, then the cancellation.
		if t0, ok := at[models.OrderConfirmed]; ok {
			steps = append(steps, TimelineStep{Key: "confirmed", Label: "Confirmed by seller", Done: true, At: ptr(t0)})
		}
		if t0, ok := at[models.OrderShipped]; ok {
			steps = append(steps, TimelineStep{Key: "shipped", Label: "Shipped", Done: true, At: ptr(t0)})
		}
		return append(steps, TimelineStep{Key: "cancelled", Label: "Cancelled", Done: true, At: ptr(t)})
	}

	rank := map[models.OrderStatus]int{
		models.OrderPending: 0, models.OrderConfirmed: 1, models.OrderShipped: 2, models.OrderDelivered: 3,
	}
	cur := rank[o.Status]
	shippedAt := at[models.OrderShipped]
	if booked != nil && shippedAt.IsZero() {
		shippedAt = booked.UpdatedAt
	}
	stages := []struct {
		st    models.OrderStatus
		key   string
		label string
		t     time.Time
	}{
		{models.OrderConfirmed, "confirmed", "Confirmed by seller", at[models.OrderConfirmed]},
		{models.OrderShipped, "shipped", "Shipped with courier", shippedAt},
		{models.OrderDelivered, "delivered", "Delivered", at[models.OrderDelivered]},
	}
	for _, st := range stages {
		done := cur >= rank[st.st]
		step := TimelineStep{Key: st.key, Label: st.label, Done: done}
		if done {
			step.At = ptr(st.t)
		}
		steps = append(steps, step)
	}
	return steps
}

// AllowedIssueTypes lists the problem kinds that make sense for this order now.
func AllowedIssueTypes(o *models.Order) []string {
	var out []string
	for _, t := range issueOrder {
		if issueRules[t](o) {
			out = append(out, t)
		}
	}
	return out
}

func safeHTTPS(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return u.String()
}

// ------------------------------------------------------------------ issues

func (s *BuyerOrderService) ReportIssue(ctx context.Context, userID, orderID, kind, message string) (*models.OrderIssue, error) {
	kind = strings.TrimSpace(kind)
	message = strings.TrimSpace(message)
	fields := map[string]string{}
	rule, known := issueRules[kind]
	if !known {
		fields["type"] = "Choose what went wrong"
	}
	if n := utf8.RuneCountInString(message); n < issueMinMessage {
		fields["message"] = "Please describe the problem (at least 10 characters)"
	} else if n > issueMaxMessage {
		fields["message"] = "Message is too long (max 1000 characters)"
	}
	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}

	email, verified, err := s.verifiedEmail(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, apperr.NotFound()
	}
	o, err := s.orders.FindByEmailAndID(ctx, email, orderID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	if !rule(o) {
		return nil, apperr.Validation(map[string]string{"type": "This can't be reported for an order in its current state"})
	}

	now := s.now().UTC()
	issue := &models.OrderIssue{
		ID: models.NewID(), OrderID: o.ID, SellerID: o.SellerID, BuyerID: userID,
		OrderNumber: o.Number, Type: kind, Message: message,
		Status: models.IssueOpen, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.issues.Create(ctx, issue); err != nil {
		if errors.Is(err, repository.ErrIssueOpen) {
			return nil, apperr.Conflict("issue_open", "You already have an open problem report for this order", "")
		}
		return nil, err
	}
	s.notifySeller(ctx, o, issue)
	return issue, nil
}

func (s *BuyerOrderService) Issues(ctx context.Context, userID string) ([]*models.OrderIssue, error) {
	return s.issues.ListByBuyer(ctx, userID, buyerIssuesLimit)
}

func (s *BuyerOrderService) WithdrawIssue(ctx context.Context, userID, issueID string) error {
	if err := s.issues.Withdraw(ctx, userID, issueID, s.now().UTC()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	return nil
}

var issueTitles = map[string]string{
	models.IssueNotReceived: "Buyer says the order has not arrived",
	models.IssueWrongItem:   "Buyer received the wrong item",
	models.IssueDamaged:     "Buyer received a damaged item",
	models.IssueMissingItem: "Buyer says an item is missing",
	models.IssueRefund:      "Buyer asked for a refund",
	models.IssueOther:       "Buyer reported a problem",
}

// notifySeller drops a bell notification on the seller's dashboard. It never
// fails the buyer's report: if it cannot be sent, the issue is still saved.
func (s *BuyerOrderService) notifySeller(ctx context.Context, o *models.Order, i *models.OrderIssue) {
	if s.notifs == nil {
		return
	}
	link := ""
	if sd, err := s.stores.GetBySeller(ctx, o.SellerID); err == nil && sd != nil && sd.Slug != "" {
		link = "/seller/" + sd.Slug + "/orders/" + o.ID
	}
	n := &models.Notification{
		ID:           models.NewID(),
		Title:        issueTitles[i.Type] + " (" + o.Number + ")",
		Body:         i.Message,
		Level:        models.NotificationWarning,
		LinkURL:      link,
		Audience:     models.NotificationAudienceSelected,
		RecipientIDs: []string{o.SellerID},
		CreatedBy:    systemSender,
		CreatedAt:    i.CreatedAt,
	}
	if err := s.notifs.Create(ctx, n); err != nil {
		s.log.Warn("buyer issue: seller notification failed", "orderId", o.ID, "error", err.Error())
	}
}
