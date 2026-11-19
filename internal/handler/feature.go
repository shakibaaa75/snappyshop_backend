package handler

import (
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

// FeatureHandler serves one-time premium features (Try the Look, ...):
// seller status and checkout, the gateway callbacks, and the admin controls.
type FeatureHandler struct {
	Svc         *service.FeatureService
	Log         *slog.Logger
	FrontendURL string
	SellerInfo  SellerInfoFunc  // optional
	CurrentSlug CurrentSlugFunc // optional
}

// featureKey reads {feature} from the URL. The URL uses dashes
// ("try-the-look"), the database uses underscores ("try_the_look").
func featureKey(r *http.Request) string {
	return strings.ReplaceAll(strings.ToLower(chi.URLParam(r, "feature")), "-", "_")
}

// featurePagePath maps a feature to the seller page the gateway returns to.
func featurePagePath(feature string) string {
	if feature == models.FeatureTryTheLook {
		return "try-the-look"
	}
	return "dashboard"
}

/* ------------------------------ seller ------------------------------ */

// Get - GET /api/v1/seller/features/{feature}
func (h *FeatureHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	view, err := h.Svc.Get(r.Context(), claims.UserID, featureKey(r))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type featureCheckoutBody struct {
	Method string `json:"method"`
	Slug   string `json:"slug"`
}

// Checkout - POST /api/v1/seller/features/{feature}/checkout
func (h *FeatureHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	var body featureCheckoutBody
	if err := httpx.DecodeJSON(w, r, &body, 1<<12); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	in := service.FeatureCheckoutInput{
		SellerID: claims.UserID,
		Feature:  featureKey(r),
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

/* ------------------- gateway callbacks (public) -------------------- */

func (h *FeatureHandler) redirect(w http.ResponseWriter, r *http.Request, p *models.FeaturePayment, result string) {
	base := strings.TrimRight(h.FrontendURL, "/")
	target := base + "/"
	if p != nil {
		slug := p.Slug
		if h.CurrentSlug != nil {
			slug = h.CurrentSlug(r.Context(), p.SellerID)
		}
		if slug != "" {
			target = base + "/seller/" + slug + "/" + featurePagePath(p.Feature) + "?status=" + result
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// CallbackSuccess - POST /api/v1/features/callback/success
func (h *FeatureHandler) CallbackSuccess(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	tranID := r.FormValue("tran_id")
	valID := r.FormValue("val_id")

	if st := formStatus(r); st != "VALID" && st != "VALIDATED" {
		p, _ := h.Svc.FailPayment(r.Context(), tranID, models.SubPayFailed)
		h.redirect(w, r, p, "failed")
		return
	}
	p, err := h.Svc.ConfirmPayment(r.Context(), tranID, valID)
	if err != nil {
		if h.Log != nil {
			h.Log.Warn("feature success callback not confirmed", "error", err.Error(), "tranId", tranID)
		}
		h.redirect(w, r, p, "failed")
		return
	}
	h.redirect(w, r, p, "success")
}

// CallbackFail - POST /api/v1/features/callback/fail
func (h *FeatureHandler) CallbackFail(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	p, _ := h.Svc.FailPayment(r.Context(), r.FormValue("tran_id"), models.SubPayFailed)
	h.redirect(w, r, p, "failed")
}

// CallbackCancel - POST /api/v1/features/callback/cancel
func (h *FeatureHandler) CallbackCancel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	p, _ := h.Svc.FailPayment(r.Context(), r.FormValue("tran_id"), models.SubPayCancelled)
	h.redirect(w, r, p, "cancelled")
}

// IPN - POST /api/v1/features/ipn (server-to-server notification)
func (h *FeatureHandler) IPN(w http.ResponseWriter, r *http.Request) {
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

/* ------------------------------ admin ------------------------------- */

type featureAdminBody struct {
	Status string `json:"status"` // disabled | revoked (status endpoint only)
	Reason string `json:"reason"`
}

// AdminGet - GET /api/v1/admin/sellers/{id}/features/{feature}
func (h *FeatureHandler) AdminGet(w http.ResponseWriter, r *http.Request) {
	view, err := h.Svc.AdminGet(r.Context(), chi.URLParam(r, "id"), featureKey(r))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// AdminGrant - POST /api/v1/admin/sellers/{id}/features/{feature}/grant
// Gives the feature for free, or switches it back on.
func (h *FeatureHandler) AdminGrant(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	var body featureAdminBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	view, err := h.Svc.AdminGrant(r.Context(), adminID, chi.URLParam(r, "id"), featureKey(r), body.Reason, clientIP(r))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// AdminSetStatus - POST /api/v1/admin/sellers/{id}/features/{feature}/status
// Body: {"status":"disabled"|"revoked","reason":"..."}
func (h *FeatureHandler) AdminSetStatus(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	var body featureAdminBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	view, err := h.Svc.AdminSetStatus(r.Context(), adminID, chi.URLParam(r, "id"), featureKey(r),
		strings.TrimSpace(strings.ToLower(body.Status)), body.Reason, clientIP(r))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}
