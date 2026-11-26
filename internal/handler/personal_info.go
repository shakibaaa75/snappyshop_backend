package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

// avatarKind is the upload kind (see storage.Kinds) for personal profile pictures.
const avatarKind = "avatar"

// PersonalInfoHandler serves the seller Settings > Personal information
// endpoints. Everything here is about the owner's private contact details, not
// the public storefront contact.
type PersonalInfoHandler struct {
	Svc *service.PersonalInfoService
	Log *slog.Logger
	// Profile picture. Both nil = pictures are switched off (R2 not configured).
	Uploads UploadStore
	R2      ObjectStore
}

// PersonalInfoDTO is what the frontend receives.
type PersonalInfoDTO struct {
	Name          string `json:"name"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"emailVerified"`
	Phone         string `json:"phone"` // 01XXXXXXXXX or ""
	PhoneVerified bool   `json:"phoneVerified"`
	AvatarURL     string `json:"avatarUrl"`
	// HasPassword false = Google / Apple only: set a password before changing
	// email or phone.
	HasPassword bool `json:"hasPassword"`
	// Which verified changes this server can do right now (see the 503 codes
	// EMAIL_NOT_CONFIGURED / SMS_NOT_CONFIGURED / OTP_STORE_NOT_CONFIGURED).
	Availability struct {
		Email bool `json:"email"`
		Phone bool `json:"phone"`
	} `json:"availability"`
}

type personalResponse struct {
	Personal PersonalInfoDTO `json:"personal"`
}

func (h *PersonalInfoHandler) dto(p *service.PersonalInfo) PersonalInfoDTO {
	a := h.Svc.Availability()
	d := PersonalInfoDTO{
		Name: p.Name, Email: p.Email, EmailVerified: p.EmailVerified,
		Phone: p.Phone, PhoneVerified: p.PhoneVerified,
		AvatarURL: p.AvatarURL, HasPassword: p.HasPassword,
	}
	d.Availability.Email, d.Availability.Phone = a.Email, a.Phone
	return d
}

func (h *PersonalInfoHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *PersonalInfoHandler) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return "", false
	}
	return claims.UserID, true
}

func (h *PersonalInfoHandler) respond(w http.ResponseWriter, p *service.PersonalInfo) {
	httpx.WriteJSON(w, http.StatusOK, personalResponse{Personal: h.dto(p)})
}

func (h *PersonalInfoHandler) resent(w http.ResponseWriter, secs int) {
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"resendAfter": secs})
}

// Get handles GET /seller/personal.
func (h *PersonalInfoHandler) Get(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	p, err := h.Svc.Get(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, p)
}

type emailChangeRequest struct {
	NewEmail        string `json:"newEmail"`
	CurrentPassword string `json:"currentPassword"`
}

type emailChangeConfirm struct {
	NewEmail string `json:"newEmail"`
	Code     string `json:"code"`
}

type codeOnly struct {
	Code string `json:"code"`
}

type phoneChangeRequest struct {
	Phone           string `json:"phone"`
	CurrentPassword string `json:"currentPassword"`
}

type phoneChangeConfirm struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

// RequestEmailChange handles POST /seller/personal/email/change.
func (h *PersonalInfoHandler) RequestEmailChange(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var req emailChangeRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	secs, err := h.Svc.RequestEmailChange(r.Context(), uid, req.CurrentPassword, req.NewEmail)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.resent(w, secs)
}

// ConfirmEmailChange handles POST /seller/personal/email/change/confirm.
func (h *PersonalInfoHandler) ConfirmEmailChange(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var req emailChangeConfirm
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Svc.ConfirmEmailChange(r.Context(), uid, req.NewEmail, req.Code)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, p)
}

// RequestEmailVerify handles POST /seller/personal/email/verify.
func (h *PersonalInfoHandler) RequestEmailVerify(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	secs, err := h.Svc.RequestEmailVerify(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.resent(w, secs)
}

// ConfirmEmailVerify handles POST /seller/personal/email/verify/confirm.
func (h *PersonalInfoHandler) ConfirmEmailVerify(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var req codeOnly
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Svc.ConfirmEmailVerify(r.Context(), uid, req.Code)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, p)
}

// RequestPhoneChange handles POST /seller/personal/phone/change.
func (h *PersonalInfoHandler) RequestPhoneChange(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var req phoneChangeRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	secs, err := h.Svc.RequestPhoneChange(r.Context(), uid, req.CurrentPassword, req.Phone)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.resent(w, secs)
}

// ConfirmPhoneChange handles POST /seller/personal/phone/change/confirm.
func (h *PersonalInfoHandler) ConfirmPhoneChange(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var req phoneChangeConfirm
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Svc.ConfirmPhoneChange(r.Context(), uid, req.Phone, req.Code)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, p)
}

type avatarRequest struct {
	UploadID string `json:"uploadId"`
}

// SetAvatar handles PUT /seller/personal/avatar.
//
// The browser first uploads the picture with POST /seller/uploads (kind
// "avatar"), which checks, re-encodes and stores it, and then sends the upload
// id here. The server looks the upload up itself, so a client can never attach
// an arbitrary URL or somebody else's file.
func (h *PersonalInfoHandler) SetAvatar(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	if h.Uploads == nil || h.R2 == nil {
		h.fail(w, r, apperr.New(http.StatusServiceUnavailable, "UPLOADS_OFF", "Image uploads are unavailable"))
		return
	}
	var req avatarRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if len(req.UploadID) > 64 || !safeID(req.UploadID) {
		h.fail(w, r, apperr.BadRequest("Invalid image"))
		return
	}
	rec, err := h.Uploads.GetOwned(r.Context(), uid, req.UploadID)
	if err != nil {
		h.fail(w, r, apperr.NotFound()) // not yours and not found look the same
		return
	}
	if rec.Kind != avatarKind || rec.Status != uploads.StatusApproved || !storage.ValidKey(rec.Key) {
		h.fail(w, r, apperr.BadRequest("Invalid image"))
		return
	}
	url := h.R2.PublicURL(rec.Key)
	prev, err := h.Svc.SetAvatar(r.Context(), uid, url)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if prev != "" && prev != url {
		h.dropOldAvatar(r.Context(), uid, prev)
	}
	h.respondFresh(w, r, uid)
}

// RemoveAvatar handles DELETE /seller/personal/avatar.
func (h *PersonalInfoHandler) RemoveAvatar(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	prev, err := h.Svc.SetAvatar(r.Context(), uid, "")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if prev != "" && h.Uploads != nil && h.R2 != nil {
		h.dropOldAvatar(r.Context(), uid, prev)
	}
	h.respondFresh(w, r, uid)
}

func (h *PersonalInfoHandler) respondFresh(w http.ResponseWriter, r *http.Request, uid string) {
	p, err := h.Svc.Get(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, p)
}

// dropOldAvatar deletes the replaced picture from R2 and its upload record. It
// only touches a file that is in the avatar folder under the caller's own
// folder, and it is best effort: the account already points at the new picture,
// so a failure is logged and never reported to the user.
func (h *PersonalInfoHandler) dropOldAvatar(ctx context.Context, uid, oldURL string) {
	key := strings.TrimPrefix(oldURL, h.R2.PublicURL(""))
	if key == oldURL || !storage.ValidKey(key) {
		return
	}
	parts := strings.Split(key, "/")
	if parts[0] != storage.Kinds[avatarKind].Folder || parts[1] != storage.OwnerSegment(uid) {
		return
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := h.R2.DeleteObject(bg, key); err != nil {
		h.Log.Warn("old avatar not deleted", "owner", uid, "error", err.Error())
		return
	}
	if rec, err := h.Uploads.GetByKeyOwned(bg, uid, key); err == nil {
		if err := h.Uploads.Delete(bg, uid, rec.ID); err != nil && err != uploads.ErrNotFound {
			h.Log.Warn("old avatar record not deleted", "owner", uid, "error", err.Error())
		}
	}
}
