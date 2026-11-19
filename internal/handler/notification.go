package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
)

// NotificationHandler serves both sides of the notification feature:
// admins send and manage them, sellers read them. The user ID always comes
// from the authenticated claims, never from the request.
type NotificationHandler struct {
	Svc *service.NotificationService
	Log *slog.Logger
}

const notificationBodyLimit = 16 << 10

func (h *NotificationHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *NotificationHandler) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return "", false
	}
	return claims.UserID, true
}

// ------------------------------------------------------------------ admin

type sendNotificationBody struct {
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Level     string     `json:"level"`
	LinkURL   string     `json:"linkUrl"`
	Audience  string     `json:"audience"`
	SellerIDs []string   `json:"sellerIds"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type adminNotificationJSON struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Level          string     `json:"level"`
	LinkURL        string     `json:"linkUrl,omitempty"`
	Audience       string     `json:"audience"`
	RecipientIDs   []string   `json:"recipientIds,omitempty"`
	CreatedBy      string     `json:"createdBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	ReadCount      int64      `json:"readCount"`
	RecipientCount int64      `json:"recipientCount"`
}

func toAdminNotificationJSON(n service.AdminNotification) adminNotificationJSON {
	return adminNotificationJSON{
		ID: n.ID, Title: n.Title, Body: n.Body, Level: n.Level, LinkURL: n.LinkURL,
		Audience: n.Audience, RecipientIDs: n.RecipientIDs, CreatedBy: n.CreatedBy,
		CreatedAt: n.CreatedAt, ExpiresAt: n.ExpiresAt,
		ReadCount: n.ReadCount, RecipientCount: n.RecipientCount,
	}
}

// Send — POST /api/v1/admin/notifications
func (h *NotificationHandler) Send(w http.ResponseWriter, r *http.Request) {
	adminID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var body sendNotificationBody
	if err := httpx.DecodeJSON(w, r, &body, notificationBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.Svc.Send(r.Context(), service.SendNotificationInput{
		Title: body.Title, Body: body.Body, Level: body.Level, LinkURL: body.LinkURL,
		Audience: body.Audience, SellerIDs: body.SellerIDs, ExpiresAt: body.ExpiresAt,
		AdminID: adminID, IP: clientIP(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"notification": toAdminNotificationJSON(service.AdminNotification{Notification: n}),
	})
}

// AdminList — GET /api/v1/admin/notifications?limit=&skip=
func (h *NotificationHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, skip := queryInt(q.Get("limit"), 20), queryInt(q.Get("skip"), 0)
	items, total, err := h.Svc.ListAdmin(r.Context(), limit, skip)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]adminNotificationJSON, 0, len(items))
	for _, n := range items {
		out = append(out, toAdminNotificationJSON(n))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"notifications": out, "total": total, "limit": limit, "skip": skip,
	})
}

// AdminDelete — DELETE /api/v1/admin/notifications/{id}
func (h *NotificationHandler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	adminID, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.Delete(r.Context(), chi.URLParam(r, "id"), adminID, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// ----------------------------------------------------------------- seller

type sellerNotificationJSON struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Level     string    `json:"level"`
	LinkURL   string    `json:"linkUrl,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Read      bool      `json:"read"`
}

// SellerList — GET /api/v1/seller/notifications
func (h *NotificationHandler) SellerList(w http.ResponseWriter, r *http.Request) {
	id, ok := h.userID(w, r)
	if !ok {
		return
	}
	items, unread, err := h.Svc.ListForSeller(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]sellerNotificationJSON, 0, len(items))
	for _, n := range items {
		out = append(out, sellerNotificationJSON{
			ID: n.ID, Title: n.Title, Body: n.Body, Level: n.Level,
			LinkURL: n.LinkURL, CreatedAt: n.CreatedAt, Read: n.Read,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "unread": unread})
}

// SellerMarkRead — POST /api/v1/seller/notifications/{id}/read
func (h *NotificationHandler) SellerMarkRead(w http.ResponseWriter, r *http.Request) {
	id, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.MarkRead(r.Context(), id, chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// SellerMarkAllRead — POST /api/v1/seller/notifications/read-all
func (h *NotificationHandler) SellerMarkAllRead(w http.ResponseWriter, r *http.Request) {
	id, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.MarkAllRead(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}
