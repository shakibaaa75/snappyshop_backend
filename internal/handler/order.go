package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

type OrderHandler struct {
	Svc *service.OrderService
	Log *slog.Logger
	// Analytics is optional (nil = no conversion tracking). When set, every
	// placed order is recorded as a "purchase" event for the seller.
	Analytics *service.AnalyticsService
	// TrustProxy must match cfg.TrustProxy (used for the client IP).
	TrustProxy bool
	// Cache is the shared cache (Redis). When set, a placed order drops the
	// seller's cached public product list, so stock shown on the storefront
	// follows the order at once. Nil = nothing to invalidate.
	Cache *cache.Cache
}

/* ------------------------------------------------------------------ */
/* DTO types                                                           */
/* ------------------------------------------------------------------ */

type OrderLineDTO struct {
	ProductID string  `json:"productId"`
	Name      string  `json:"name"`
	ImageURL  string  `json:"imageUrl,omitempty"`
	Emoji     string  `json:"emoji,omitempty"`
	Hue       int     `json:"hue,omitempty"`
	VariantID string  `json:"variantId,omitempty"`
	Variant   string  `json:"variant,omitempty"`
	Price     float64 `json:"price"`
	Qty       int     `json:"qty"`
	Subtotal  float64 `json:"subtotal"`
}

type OrderEventDTO struct {
	Status string `json:"status"`
	At     string `json:"at"`
	Note   string `json:"note,omitempty"`
}

type OrderDTO struct {
	ID              string  `json:"id"`
	Number          string  `json:"number"`
	CustomerName    string  `json:"customerName"`
	CustomerPhone   string  `json:"customerPhone"`
	CustomerEmail   string  `json:"customerEmail,omitempty"`
	ShippingAddress string  `json:"shippingAddress"`
	ShippingCity    string  `json:"shippingCity"`
	ShippingArea    string  `json:"shippingArea,omitempty"`
	Notes           string  `json:"notes,omitempty"`
	PaymentMethod   string  `json:"paymentMethod"`
	Paid            bool    `json:"paid"`
	Subtotal        float64 `json:"subtotal"`
	Delivery        float64 `json:"delivery"`
	Discount        float64 `json:"discount"`
	Total           float64 `json:"total"`
	// How delivery was priced. DeliveryZone is "inside_dhaka" or
	// "outside_dhaka"; it is empty for orders placed before Delivery Settings
	// existed. DeliveryBase is the charge before a free-delivery waiver.
	DeliveryZone   string          `json:"deliveryZone,omitempty"`
	DeliveryBase   float64         `json:"deliveryBase"`
	DeliveryWaived bool            `json:"deliveryWaived"`
	Lines          []OrderLineDTO  `json:"lines"`
	Status         string          `json:"status"`
	Events         []OrderEventDTO `json:"events"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
}

func toOrderDTO(o *models.Order) OrderDTO {
	lines := make([]OrderLineDTO, 0, len(o.Lines))
	for _, l := range o.Lines {
		lines = append(lines, OrderLineDTO{
			ProductID: l.ProductID,
			Name:      l.Name,
			ImageURL:  l.ImageURL,
			Emoji:     l.Emoji,
			Hue:       l.Hue,
			VariantID: l.VariantID,
			Variant:   l.Variant,
			Price:     l.Price,
			Qty:       l.Qty,
			Subtotal:  l.Subtotal,
		})
	}
	events := make([]OrderEventDTO, 0, len(o.Events))
	for _, e := range o.Events {
		events = append(events, OrderEventDTO{
			Status: string(e.Status),
			At:     e.At.UTC().Format(time.RFC3339),
			Note:   e.Note,
		})
	}
	return OrderDTO{
		ID:              o.ID,
		Number:          o.Number,
		CustomerName:    o.CustomerName,
		CustomerPhone:   o.CustomerPhone,
		CustomerEmail:   o.CustomerEmail,
		ShippingAddress: o.ShippingAddress,
		ShippingCity:    o.ShippingCity,
		ShippingArea:    o.ShippingArea,
		Notes:           o.Notes,
		PaymentMethod:   string(o.PaymentMethod),
		Paid:            o.Paid,
		Subtotal:        o.Subtotal,
		Delivery:        o.Delivery,
		Discount:        o.Discount,
		Total:           o.Total,
		DeliveryZone:    string(o.DeliveryZone),
		DeliveryBase:    o.DeliveryBase,
		DeliveryWaived:  o.DeliveryWaived,
		Lines:           lines,
		Status:          string(o.Status),
		Events:          events,
		CreatedAt:       o.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       o.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

/* ------------------------------------------------------------------ */
/* Request bodies                                                      */
/* ------------------------------------------------------------------ */

type createOrderLineBody struct {
	ProductID string `json:"productId"`
	VariantID string `json:"variantId"`
	Qty       int    `json:"qty"`
}

type createOrderBody struct {
	CustomerName    string                `json:"customerName"`
	CustomerPhone   string                `json:"customerPhone"`
	CustomerEmail   string                `json:"customerEmail"`
	ShippingAddress string                `json:"shippingAddress"`
	ShippingCity    string                `json:"shippingCity"`
	ShippingArea    string                `json:"shippingArea"`
	Notes           string                `json:"notes"`
	PaymentMethod   string                `json:"paymentMethod"`
	Lines           []createOrderLineBody `json:"lines"`

	// Optional attribution for conversion tracking. Never used for the order
	// itself. VisitorID is only sent when the visitor consented to the device ID.
	Source    string `json:"source"`
	VisitorID string `json:"visitorId"`
}

// quoteBody is what checkout sends to price a cart: the same lines and the
// same shippingCity field it sends when placing the order.
type quoteBody struct {
	ShippingCity string                `json:"shippingCity"`
	Lines        []createOrderLineBody `json:"lines"`
}

// DeliveryQuoteDTO is the price of a cart for one district.
type DeliveryQuoteDTO struct {
	DeliveryZone        string  `json:"deliveryZone"`
	Subtotal            float64 `json:"subtotal"`
	Delivery            float64 `json:"delivery"`
	DeliveryBase        float64 `json:"deliveryBase"`
	FreeDeliveryApplied bool    `json:"freeDeliveryApplied"`
	Discount            float64 `json:"discount"`
	Total               float64 `json:"total"`
}

type updateStatusBody struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

type markPaidBody struct {
	Paid bool `json:"paid"`
}

/* ------------------------------------------------------------------ */
/* Handlers                                                            */
/* ------------------------------------------------------------------ */

func (h *OrderHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// Create — POST /api/v1/store/{slug}/orders (public)
func (h *OrderHandler) Create(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		h.fail(w, r, apperr.BadRequest("Missing slug"))
		return
	}
	var body createOrderBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	lines := make([]service.OrderLineInput, 0, len(body.Lines))
	for _, l := range body.Lines {
		lines = append(lines, service.OrderLineInput{
			ProductID: l.ProductID,
			VariantID: l.VariantID,
			Qty:       l.Qty,
		})
	}
	order, err := h.Svc.CreateBySlug(r.Context(), slug, service.CreateOrderInput{
		CustomerName:    body.CustomerName,
		CustomerPhone:   body.CustomerPhone,
		CustomerEmail:   body.CustomerEmail,
		ShippingAddress: body.ShippingAddress,
		ShippingCity:    body.ShippingCity,
		ShippingArea:    body.ShippingArea,
		Notes:           body.Notes,
		PaymentMethod:   body.PaymentMethod,
		Lines:           lines,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.trackPurchase(r, order.SellerID, order.ID, order.Total, body.Source, body.VisitorID)
	// Stock changed: the cached public product list is out of date.
	invalidateSellerStorefront(h.Cache, order.SellerID)
	// FIX: return the full order (same shape the seller endpoints use) so the
	// confirmation page has lines, status, customer details, etc. The original
	// five fields (id, number, subtotal, delivery, total) are all still here.
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"order": toOrderDTO(order),
	})
}

// Quote — POST /api/v1/store/{slug}/delivery-quote (public)
//
// Prices a cart for the district the customer picked, with the exact rules
// Create uses, without placing an order or touching stock. Checkout calls it
// whenever the district or the cart changes.
func (h *OrderHandler) Quote(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		h.fail(w, r, apperr.BadRequest("Missing slug"))
		return
	}
	var body quoteBody
	// Lenient on purpose: checkout may post the same payload it builds for
	// the order itself, and a read-only quote should not fail over the
	// extra fields (name, phone, ...). Only shippingCity and lines are read.
	if err := httpx.DecodeJSONLenient(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	lines := make([]service.OrderLineInput, 0, len(body.Lines))
	for _, l := range body.Lines {
		lines = append(lines, service.OrderLineInput{ProductID: l.ProductID, VariantID: l.VariantID, Qty: l.Qty})
	}
	q, err := h.Svc.QuoteBySlug(r.Context(), slug, service.QuoteInput{ShippingCity: body.ShippingCity, Lines: lines})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, DeliveryQuoteDTO{
		DeliveryZone:        string(q.Zone),
		Subtotal:            q.Subtotal,
		Delivery:            q.Delivery,
		DeliveryBase:        q.DeliveryBase,
		FreeDeliveryApplied: q.FreeDeliveryApplied,
		Discount:            q.Discount,
		Total:               q.Total,
	})
}

// List — GET /api/v1/seller/orders
func (h *OrderHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	// TEMP DEBUG: remove once the seller dashboard shows orders correctly.
	h.Log.Info("seller orders list", "userId", claims.UserID)

	status := r.URL.Query().Get("status")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	items, err := h.Svc.List(r.Context(), claims.UserID, status, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]OrderDTO, 0, len(items))
	for _, o := range items {
		out = append(out, toOrderDTO(o))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// Get — GET /api/v1/seller/orders/{id}
func (h *OrderHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	order, err := h.Svc.Get(r.Context(), claims.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": toOrderDTO(order)})
}

// UpdateStatus — PATCH /api/v1/seller/orders/{id}
func (h *OrderHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	var body updateStatusBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	order, err := h.Svc.UpdateStatus(r.Context(), claims.UserID, id, body.Status, body.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": toOrderDTO(order)})
}

// MarkPaid — PATCH /api/v1/seller/orders/{id}/paid
func (h *OrderHandler) MarkPaid(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	var body markPaidBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	order, err := h.Svc.MarkPaid(r.Context(), claims.UserID, id, body.Paid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": toOrderDTO(order)})
}

// trackPurchase records the conversion for the seller's analytics. It must
// never fail or slow the checkout, so errors are only logged.
func (h *OrderHandler) trackPurchase(r *http.Request, sellerID, orderID string, total float64, source, visitorID string) {
	if h.Analytics == nil {
		return
	}
	err := h.Analytics.TrackPurchase(r.Context(), service.PurchaseInput{
		SellerID:  sellerID,
		OrderID:   orderID,
		Total:     total,
		Source:    strings.TrimSpace(source),
		VisitorID: strings.TrimSpace(visitorID),
		ClientIP:  middleware.ClientIP(r, h.TrustProxy),
		UserAgent: r.UserAgent(),
	})
	if err != nil && h.Log != nil {
		h.Log.Warn("order: purchase tracking failed", "error", err.Error(), "orderId", orderID)
	}
}
