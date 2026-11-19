package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

// SellerInfoFunc is optional. It lets the gateway receipt carry the seller's
// real name and email. Leave it nil and generic placeholders are used.
type SellerInfoFunc func(ctx context.Context, userID string) (name, email string)

// CurrentSlugFunc returns a seller's CURRENT store address ("" if none). It
// keeps payment redirects and receipts from ever using a retired address.
type CurrentSlugFunc func(ctx context.Context, sellerID string) string

type SubscriptionHandler struct {
	Svc         *service.SubscriptionService
	Grants      *service.SubscriptionGrantService // free-plan offers; nil disables claiming
	Log         *slog.Logger
	FrontendURL string // e.g. http://localhost:3000
	SellerInfo  SellerInfoFunc
	CurrentSlug CurrentSlugFunc
}

type checkoutBody struct {
	Plan     string `json:"plan"`
	Interval string `json:"interval"`
	Method   string `json:"method"`
	Slug     string `json:"slug"`
}

// Get - GET /api/v1/seller/subscription
func (h *SubscriptionHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	view, err := h.Svc.Get(r.Context(), claims.UserID)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// ClaimGrant - POST /api/v1/seller/subscription/grants/{id}/claim
// The seller accepts a free plan an admin offered. Returns the new
// subscription view (plan, entitlements, pendingGrants).
func (h *SubscriptionHandler) ClaimGrant(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	if h.Grants == nil {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	view, err := h.Grants.Claim(r.Context(), claims.UserID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// Checkout - POST /api/v1/seller/subscription/checkout
func (h *SubscriptionHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}

	var body checkoutBody
	if err := httpx.DecodeJSON(w, r, &body, 1<<12); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}

	in := service.CheckoutInput{
		SellerID: claims.UserID,
		Plan:     strings.TrimSpace(strings.ToLower(body.Plan)),
		Interval: strings.TrimSpace(strings.ToLower(body.Interval)),
		Method:   strings.TrimSpace(strings.ToLower(body.Method)),
		Slug:     strings.TrimSpace(strings.ToLower(body.Slug)),
	}
	if h.SellerInfo != nil {
		in.SellerName, in.SellerEmail = h.SellerInfo(r.Context(), claims.UserID)
	}
	// Never trust a client-supplied address: use the seller's current one.
	if h.CurrentSlug != nil {
		if cur := h.CurrentSlug(r.Context(), claims.UserID); cur != "" {
			in.Slug = cur
		}
	}

	payURL, err := h.Svc.Checkout(r.Context(), in)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"paymentUrl": payURL})
}

/* ------------------------------------------------------------------ */
/* Gateway callbacks (public, form-encoded, no login, no CSRF)         */
/* ------------------------------------------------------------------ */

func (h *SubscriptionHandler) redirect(w http.ResponseWriter, r *http.Request, p *models.SubscriptionPayment, result string) {
	base := strings.TrimRight(h.FrontendURL, "/")
	target := base + "/"
	if p != nil {
		// Resolve the address from the seller, not from the stored copy, so a
		// shop renamed while the payment was open never redirects to an old URL.
		slug := p.Slug
		if h.CurrentSlug != nil {
			slug = h.CurrentSlug(r.Context(), p.SellerID)
		}
		if slug != "" {
			target = base + "/seller/" + slug + "/subscription?status=" + result
		}
	}
	// 303 turns the gateway's POST into a GET for the browser.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func formStatus(r *http.Request) string {
	return strings.ToUpper(strings.TrimSpace(r.FormValue("status")))
}

// CallbackSuccess - POST /api/v1/subscription/callback/success
func (h *SubscriptionHandler) CallbackSuccess(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	tranID := r.FormValue("tran_id")
	valID := r.FormValue("val_id")

	st := formStatus(r)
	if st != "VALID" && st != "VALIDATED" {
		p, _ := h.Svc.FailPayment(r.Context(), tranID, models.SubPayFailed)
		h.redirect(w, r, p, "failed")
		return
	}

	p, err := h.Svc.ConfirmPayment(r.Context(), tranID, valID)
	if err != nil {
		if h.Log != nil {
			h.Log.Warn("subscription success callback not confirmed", "error", err.Error(), "tranId", tranID)
		}
		h.redirect(w, r, p, "failed")
		return
	}
	h.redirect(w, r, p, "success")
}

// CallbackFail - POST /api/v1/subscription/callback/fail
func (h *SubscriptionHandler) CallbackFail(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	p, _ := h.Svc.FailPayment(r.Context(), r.FormValue("tran_id"), models.SubPayFailed)
	h.redirect(w, r, p, "failed")
}

// CallbackCancel - POST /api/v1/subscription/callback/cancel
func (h *SubscriptionHandler) CallbackCancel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	p, _ := h.Svc.FailPayment(r.Context(), r.FormValue("tran_id"), models.SubPayCancelled)
	h.redirect(w, r, p, "cancelled")
}

// IPN - POST /api/v1/subscription/ipn (server-to-server notification)
func (h *SubscriptionHandler) IPN(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	tranID := r.FormValue("tran_id")

	switch formStatus(r) {
	case "VALID", "VALIDATED":
		if _, err := h.Svc.ConfirmPayment(r.Context(), tranID, r.FormValue("val_id")); err != nil {
			httpx.WriteError(w, r, err, h.Log)
			return
		}
	case "CANCELLED":
		_, _ = h.Svc.FailPayment(r.Context(), tranID, models.SubPayCancelled)
	case "FAILED", "EXPIRED", "UNATTEMPTED":
		_, _ = h.Svc.FailPayment(r.Context(), tranID, models.SubPayFailed)
	}
	w.WriteHeader(http.StatusOK)
}
