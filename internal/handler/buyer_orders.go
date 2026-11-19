package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

// BuyerOrderHandler serves "My orders" and "Problem with this order".
// The buyer always comes from the sign-in claims.
type BuyerOrderHandler struct {
	Svc *service.BuyerOrderService
	Log *slog.Logger
}

func (h *BuyerOrderHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *BuyerOrderHandler) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return "", false
	}
	return claims.UserID, true
}

// ------------------------------------------------------------------ JSON

type orderLineJSON struct {
	Name     string  `json:"name"`
	ImageURL string  `json:"imageUrl"`
	Emoji    string  `json:"emoji"`
	Hue      int     `json:"hue"`
	Variant  string  `json:"variant"`
	Price    float64 `json:"price"`
	Qty      int     `json:"qty"`
	Subtotal float64 `json:"subtotal"`
}

type sellerRefJSON struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type orderSummaryJSON struct {
	ID           string          `json:"id"`
	Number       string          `json:"number"`
	Status       string          `json:"status"`
	CreatedAt    time.Time       `json:"createdAt"`
	Total        float64         `json:"total"`
	ItemCount    int             `json:"itemCount"`
	Preview      []orderLineJSON `json:"preview"`
	Seller       sellerRefJSON   `json:"seller"`
	HasOpenIssue bool            `json:"hasOpenIssue"`
}

type timelineStepJSON struct {
	Key   string     `json:"key"`
	Label string     `json:"label"`
	Done  bool       `json:"done"`
	At    *time.Time `json:"at"`
}

type trackingJSON struct {
	Courier string `json:"courier"`
	Code    string `json:"code"`
	URL     string `json:"url"`
}

type issueJSON struct {
	ID          string     `json:"id"`
	OrderID     string     `json:"orderId"`
	OrderNumber string     `json:"orderNumber"`
	Type        string     `json:"type"`
	Message     string     `json:"message"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ClosedAt    *time.Time `json:"closedAt"`
}

type orderDetailJSON struct {
	orderSummaryJSON
	Lines          []orderLineJSON    `json:"lines"`
	Subtotal       float64            `json:"subtotal"`
	Delivery       float64            `json:"delivery"`
	Discount       float64            `json:"discount"`
	DeliveryZone   string             `json:"deliveryZone,omitempty"`
	DeliveryBase   float64            `json:"deliveryBase"`
	DeliveryWaived bool               `json:"deliveryWaived"`
	PaymentMethod  string             `json:"paymentMethod"`
	Paid           bool               `json:"paid"`
	CustomerName   string             `json:"customerName"`
	Phone          string             `json:"phone"`
	Address        string             `json:"address"`
	City           string             `json:"city"`
	Area           string             `json:"area"`
	Timeline       []timelineStepJSON `json:"timeline"`
	Tracking       *trackingJSON      `json:"tracking"`
	Issues         []issueJSON        `json:"issues"`
	ReportTypes    []string           `json:"reportTypes"`
}

func toLineJSON(l service.BuyerOrderLine) orderLineJSON {
	return orderLineJSON{
		Name: l.Name, ImageURL: l.ImageURL, Emoji: l.Emoji, Hue: l.Hue,
		Variant: l.Variant, Price: l.Price, Qty: l.Qty, Subtotal: l.Subtotal,
	}
}

func toSummaryJSON(s service.BuyerOrderSummary) orderSummaryJSON {
	prev := make([]orderLineJSON, 0, len(s.Preview))
	for _, l := range s.Preview {
		prev = append(prev, toLineJSON(l))
	}
	return orderSummaryJSON{
		ID: s.ID, Number: s.Number, Status: string(s.Status), CreatedAt: s.CreatedAt,
		Total: s.Total, ItemCount: s.ItemCount, Preview: prev,
		Seller:       sellerRefJSON{Name: s.Seller.Name, Slug: s.Seller.Slug},
		HasOpenIssue: s.HasOpenIssue,
	}
}

func toIssueJSON(i *models.OrderIssue) issueJSON {
	return issueJSON{
		ID: i.ID, OrderID: i.OrderID, OrderNumber: i.OrderNumber, Type: i.Type,
		Message: i.Message, Status: i.Status, CreatedAt: i.CreatedAt,
		UpdatedAt: i.UpdatedAt, ClosedAt: i.ClosedAt,
	}
}

func toDetailJSON(d *service.BuyerOrderDetail) orderDetailJSON {
	lines := make([]orderLineJSON, 0, len(d.Lines))
	for _, l := range d.Lines {
		lines = append(lines, toLineJSON(l))
	}
	steps := make([]timelineStepJSON, 0, len(d.Timeline))
	for _, s := range d.Timeline {
		steps = append(steps, timelineStepJSON{Key: s.Key, Label: s.Label, Done: s.Done, At: s.At})
	}
	issues := make([]issueJSON, 0, len(d.Issues))
	for _, i := range d.Issues {
		issues = append(issues, toIssueJSON(i))
	}
	types := d.ReportTypes
	if types == nil {
		types = []string{}
	}
	out := orderDetailJSON{
		orderSummaryJSON: toSummaryJSON(d.BuyerOrderSummary),
		Lines:            lines, Subtotal: d.Subtotal, Delivery: d.Delivery,
		Discount: d.Discount, DeliveryZone: string(d.DeliveryZone),
		DeliveryBase: d.DeliveryBase, DeliveryWaived: d.DeliveryWaived,
		PaymentMethod: string(d.PaymentMethod), Paid: d.Paid,
		CustomerName: d.CustomerName, Phone: d.Phone,
		Address: d.Address, City: d.City, Area: d.Area,
		Timeline: steps, Issues: issues, ReportTypes: types,
	}
	if d.Tracking != nil {
		out.Tracking = &trackingJSON{Courier: d.Tracking.Courier, Code: d.Tracking.Code, URL: d.Tracking.URL}
	}
	return out
}

// ------------------------------------------------------------------ routes

// Orders — GET /api/v1/buyer/orders?limit=20&before=<RFC3339 time>
func (h *BuyerOrderHandler) Orders(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			h.fail(w, r, apperr.BadRequest("limit must be a positive number"))
			return
		}
		limit = n
	}
	var before time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			h.fail(w, r, apperr.BadRequest("before must be an RFC 3339 time"))
			return
		}
		before = t
	}
	list, err := h.Svc.Orders(r.Context(), uid, limit, before)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	orders := make([]orderSummaryJSON, 0, len(list.Orders))
	for _, o := range list.Orders {
		orders = append(orders, toSummaryJSON(o))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"orders":        orders,
		"nextBefore":    list.NextBefore,
		"emailVerified": list.EmailVerified,
	})
}

// Order — GET /api/v1/buyer/orders/{id}
func (h *BuyerOrderHandler) Order(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	d, err := h.Svc.Order(r.Context(), uid, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": toDetailJSON(d)})
}

type reportIssueBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ReportIssue — POST /api/v1/buyer/orders/{id}/issues
func (h *BuyerOrderHandler) ReportIssue(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var b reportIssueBody
	if err := httpx.DecodeJSON(w, r, &b, 8<<10); err != nil {
		h.fail(w, r, err)
		return
	}
	i, err := h.Svc.ReportIssue(r.Context(), uid, chi.URLParam(r, "id"), b.Type, b.Message)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"issue": toIssueJSON(i)})
}

// Issues — GET /api/v1/buyer/issues
func (h *BuyerOrderHandler) Issues(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	list, err := h.Svc.Issues(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]issueJSON, 0, len(list))
	for _, i := range list {
		out = append(out, toIssueJSON(i))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"issues": out})
}

// WithdrawIssue — POST /api/v1/buyer/issues/{id}/withdraw
func (h *BuyerOrderHandler) WithdrawIssue(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.WithdrawIssue(r.Context(), uid, chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
