package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

// BuyerHandler serves the buyer's own profile and saved addresses.
// The user ID always comes from the authenticated claims.
type BuyerHandler struct {
	Svc *service.BuyerService
	Log *slog.Logger
}

const buyerBodyLimit = 8 << 10

func (h *BuyerHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *BuyerHandler) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return "", false
	}
	return claims.UserID, true
}

type buyerProfileJSON struct {
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	Phone        string    `json:"phone"`
	AvatarURL    string    `json:"avatarUrl"`
	Bio          string    `json:"bio"`
	Language     string    `json:"language"`
	HomeTheme    string    `json:"homeTheme"`
	MemberSince  time.Time `json:"memberSince"`
	AddressCount int       `json:"addressCount"`
}

func toBuyerProfileJSON(v *service.BuyerProfileView) buyerProfileJSON {
	return buyerProfileJSON{
		Name: v.Name, Email: v.Email, Phone: v.Phone, AvatarURL: v.AvatarURL, Bio: v.Bio,
		Language: v.Language, HomeTheme: v.HomeTheme, MemberSince: v.MemberSince, AddressCount: v.AddressCount,
	}
}

type buyerAddressJSON struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	FullName   string    `json:"fullName"`
	Phone      string    `json:"phone"`
	Division   string    `json:"division"`
	District   string    `json:"district"`
	Upazila    string    `json:"upazila"`
	Area       string    `json:"area"`
	PostalCode string    `json:"postalCode"`
	IsDefault  bool      `json:"isDefault"`
	CreatedAt  time.Time `json:"createdAt"`
}

func toBuyerAddressJSON(a models.BuyerAddress) buyerAddressJSON {
	return buyerAddressJSON{
		ID: a.ID, Label: a.Label, FullName: a.FullName, Phone: a.Phone, Division: a.Division,
		District: a.District, Upazila: a.Upazila, Area: a.Area, PostalCode: a.PostalCode,
		IsDefault: a.IsDefault, CreatedAt: a.CreatedAt,
	}
}

// Profile — GET /api/v1/buyer/profile
func (h *BuyerHandler) Profile(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	v, err := h.Svc.Profile(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"profile": toBuyerProfileJSON(v)})
}

type updateBuyerProfileBody struct {
	Name      string `json:"name"`
	Phone     string `json:"phone"`
	AvatarURL string `json:"avatarUrl"`
	Bio       string `json:"bio"`
	Language  string `json:"language"`
}

// UpdateProfile — PUT /api/v1/buyer/profile
func (h *BuyerHandler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var b updateBuyerProfileBody
	if err := httpx.DecodeJSON(w, r, &b, buyerBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.UpdateProfile(r.Context(), uid, service.UpdateBuyerProfileInput{
		Name: b.Name, Phone: b.Phone, AvatarURL: b.AvatarURL, Bio: b.Bio, Language: b.Language,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"profile": toBuyerProfileJSON(v)})
}

type updateBuyerPreferencesBody struct {
	HomeTheme string `json:"homeTheme"`
}

// UpdatePreferences — PUT /api/v1/buyer/preferences
// Saves only the preferences sent here (today: the home page theme), so it can
// never overwrite the name, phone, bio or language on the profile.
func (h *BuyerHandler) UpdatePreferences(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var b updateBuyerPreferencesBody
	if err := httpx.DecodeJSON(w, r, &b, buyerBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	theme, err := h.Svc.SetHomeTheme(r.Context(), uid, b.HomeTheme)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"homeTheme": theme})
}

type buyerAddressBody struct {
	Label      string `json:"label"`
	FullName   string `json:"fullName"`
	Phone      string `json:"phone"`
	Division   string `json:"division"`
	District   string `json:"district"`
	Upazila    string `json:"upazila"`
	Area       string `json:"area"`
	PostalCode string `json:"postalCode"`
	IsDefault  bool   `json:"isDefault"`
}

func (b buyerAddressBody) input() service.BuyerAddressInput {
	return service.BuyerAddressInput{
		Label: b.Label, FullName: b.FullName, Phone: b.Phone, Division: b.Division,
		District: b.District, Upazila: b.Upazila, Area: b.Area, PostalCode: b.PostalCode,
		IsDefault: b.IsDefault,
	}
}

// Addresses — GET /api/v1/buyer/addresses
func (h *BuyerHandler) Addresses(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	list, err := h.Svc.Addresses(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]buyerAddressJSON, 0, len(list))
	for _, a := range list {
		out = append(out, toBuyerAddressJSON(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"addresses": out, "divisions": service.BangladeshDivisions,
	})
}

// AddAddress — POST /api/v1/buyer/addresses
func (h *BuyerHandler) AddAddress(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var b buyerAddressBody
	if err := httpx.DecodeJSON(w, r, &b, buyerBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	a, err := h.Svc.AddAddress(r.Context(), uid, b.input())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"address": toBuyerAddressJSON(*a)})
}

// UpdateAddress — PUT /api/v1/buyer/addresses/{id}
func (h *BuyerHandler) UpdateAddress(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	var b buyerAddressBody
	if err := httpx.DecodeJSON(w, r, &b, buyerBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	a, err := h.Svc.UpdateAddress(r.Context(), uid, chi.URLParam(r, "id"), b.input())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"address": toBuyerAddressJSON(*a)})
}

// SetDefaultAddress — PUT /api/v1/buyer/addresses/{id}/default
func (h *BuyerHandler) SetDefaultAddress(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.SetDefaultAddress(r.Context(), uid, chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DeleteAddress — DELETE /api/v1/buyer/addresses/{id}
func (h *BuyerHandler) DeleteAddress(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.DeleteAddress(r.Context(), uid, chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
