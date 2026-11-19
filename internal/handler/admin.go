package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/service"
)

type AdminHandler struct {
	Svc *service.AdminService
	Log *slog.Logger
}

func (h *AdminHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func adminClaims(r *http.Request) (string, bool) {
	c, ok := middleware.ClaimsFrom(r.Context())
	if !ok || c == nil {
		return "", false
	}
	return c.UserID, true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return r.RemoteAddr
}

func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (h *AdminHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := h.Svc.Dashboard(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sellers, reports, logs, err := h.Svc.RecentActivity(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"stats": stats, "recentSellers": mapUsers(sellers),
		"recentReports": mapReports(reports), "recentAdminLogs": mapLogs(logs),
	})
}

func (h *AdminHandler) ListSellers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := repository.UserListFilter{
		Search: strings.TrimSpace(q.Get("q")), Limit: queryInt(q.Get("limit"), 20),
		Skip: queryInt(q.Get("skip"), 0), SortNewest: true,
	}
	if st := q.Get("status"); st != "" {
		s := models.Status(st)
		filter.Status = &s
	}
	items, total, err := h.Svc.ListSellers(r.Context(), filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, mapSellerListItem(it))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sellers": out, "total": total, "limit": filter.Limit, "skip": filter.Skip})
}

func (h *AdminHandler) GetSeller(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u, sum, err := h.Svc.GetSeller(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	history, _ := h.Svc.SellerRestrictionHistory(r.Context(), id)
	notes, _ := h.Svc.ListNotes(r.Context(), "seller", id)
	promos, _ := h.Svc.SellerPromotions(r.Context(), id)
	subHist, _ := h.Svc.SellerSubscriptionHistory(r.Context(), id)
	slugHist, _ := h.Svc.SellerSlugHistory(r.Context(), id)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"seller": mapUserAdmin(u), "summary": sum,
		"restrictionHistory": mapRestrictions(history), "notes": mapNotes(notes),
		"promotions": mapPromotions(promos), "subscriptionHistory": mapOverrides(subHist),
		"slugHistory": mapSlugChanges(slugHist),
	})
}

// ListSlugHistory — GET /api/v1/admin/slug-history?q=&sellerId=&limit=&skip=
// Store-address changes of every seller (or one, with sellerId), newest first.
func (h *AdminHandler) ListSlugHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repository.SlugHistoryFilter{
		SellerID: strings.TrimSpace(q.Get("sellerId")),
		Search:   strings.TrimSpace(q.Get("q")),
		Limit:    queryInt(q.Get("limit"), 30),
		Skip:     queryInt(q.Get("skip"), 0),
	}
	items, total, err := h.Svc.ListSlugHistory(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m := mapSlugChange(it.Change)
		m["sellerName"] = it.SellerName
		m["sellerEmail"] = it.SellerEmail
		m["sellerStatus"] = string(it.SellerStatus)
		m["currentSlug"] = it.CurrentSlug
		out = append(out, m)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"changes": out, "total": total, "limit": f.Limit, "skip": f.Skip})
}

type restrictBody struct {
	Kind         string `json:"kind"`
	Reason       string `json:"reason"`
	DurationDays int    `json:"durationDays"`
}

func (h *AdminHandler) RestrictSeller(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var body restrictBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	kind := body.Kind
	if kind == "" {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/suspend"):
			kind = models.RestrictionSuspend
		case strings.HasSuffix(path, "/restrict"):
			kind = models.RestrictionRestrict
		case strings.HasSuffix(path, "/ban"):
			kind = models.RestrictionBan
		}
	}
	var dur *time.Duration
	if body.DurationDays > 0 {
		d := time.Duration(body.DurationDays) * 24 * time.Hour
		dur = &d
	}
	if err := h.Svc.RestrictSeller(r.Context(), id, service.RestrictInput{
		Kind: kind, Reason: body.Reason, Duration: dur, AdminID: adminID, IP: clientIP(r),
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (h *AdminHandler) RestoreSeller(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var body reasonBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.RestoreSeller(r.Context(), id, body.Reason, adminID, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type noteBody struct {
	Body string `json:"body"`
}

func (h *AdminHandler) AddSellerNote(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var body noteBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.Svc.AddNote(r.Context(), "seller", id, body.Body, adminID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"note": mapNote(n)})
}

func (h *AdminHandler) ListSellerNotes(w http.ResponseWriter, r *http.Request) {
	notes, err := h.Svc.ListNotes(r.Context(), "seller", chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"notes": mapNotes(notes)})
}

type grantSubBody struct {
	Plan          string `json:"plan"`
	DurationDays  int    `json:"durationDays"`
	DiscountPct   int    `json:"discountPct"`
	DiscountFixed int64  `json:"discountFixed"`
	IsPromotional bool   `json:"isPromotional"`
	Reason        string `json:"reason"`
}

func (h *AdminHandler) GrantSubscription(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var body grantSubBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.IsPromotional && body.DiscountPct == 0 && body.DiscountFixed == 0 {
		body.DiscountPct = 100
	}
	o, offer, err := h.Svc.GrantSubscription(r.Context(), id, service.GrantSubscriptionInput{
		Plan: body.Plan, DurationDays: body.DurationDays, DiscountPct: body.DiscountPct,
		DiscountFixed: body.DiscountFixed, IsPromotional: body.IsPromotional,
		Reason: body.Reason, AdminID: adminID, IP: clientIP(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if offer != nil {
		// Free plan: offered, not active yet. The seller must claim it.
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"offered": true,
			"grant": map[string]any{
				"id": offer.ID, "plan": offer.Plan, "days": offer.Days,
				"reason": offer.Reason, "status": offer.Status,
				"createdAt": offer.CreatedAt.UTC().Format(time.RFC3339),
			},
		})
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"offered": false, "override": mapOverride(o)})
}

type featureBody struct {
	Kind         string `json:"kind"`
	Priority     int    `json:"priority"`
	DurationDays int    `json:"durationDays"`
	Reason       string `json:"reason"`
}

func (h *AdminHandler) FeatureSeller(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var body featureBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	var ends *time.Time
	if body.DurationDays > 0 {
		t := time.Now().UTC().Add(time.Duration(body.DurationDays) * 24 * time.Hour)
		ends = &t
	}
	p, err := h.Svc.FeatureSeller(r.Context(), id, body.Kind, body.Reason, adminID, clientIP(r), body.Priority, ends)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"promotion": mapPromotion(p)})
}

func (h *AdminHandler) UnfeatureSeller(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var body featureBody
	_ = httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes)
	if err := h.Svc.UnfeatureSeller(r.Context(), id, body.Kind, adminID, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *AdminHandler) ListVerifications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, total, err := h.Svc.ListVerifications(r.Context(), q.Get("status"), queryInt(q.Get("limit"), 20), queryInt(q.Get("skip"), 0))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"verifications": mapVerifications(items), "total": total})
}

type reviewVerifBody struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func (h *AdminHandler) ReviewVerification(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body reviewVerifBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.ReviewVerification(r.Context(), chi.URLParam(r, "id"), body.Status, body.Reason, adminID, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *AdminHandler) ListReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, total, err := h.Svc.ListReports(r.Context(), repository.ReportListFilter{
		Status: q.Get("status"), SellerID: q.Get("sellerId"),
		Limit: queryInt(q.Get("limit"), 20), Skip: queryInt(q.Get("skip"), 0),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"reports": mapReports(items), "total": total})
}

func (h *AdminHandler) GetReport(w http.ResponseWriter, r *http.Request) {
	item, err := h.Svc.GetReport(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"report": mapReport(item)})
}

type resolveReportBody struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
}

func (h *AdminHandler) ResolveReport(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body resolveReportBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.ResolveReport(r.Context(), chi.URLParam(r, "id"), service.ResolveReportInput{
		Status: body.Status, Resolution: body.Resolution, AdminID: adminID, IP: clientIP(r),
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *AdminHandler) ListCustomers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := repository.UserListFilter{
		Search: strings.TrimSpace(q.Get("q")), Limit: queryInt(q.Get("limit"), 20),
		Skip: queryInt(q.Get("skip"), 0), SortNewest: true,
	}
	if st := q.Get("status"); st != "" {
		s := models.Status(st)
		filter.Status = &s
	}
	items, total, err := h.Svc.ListCustomers(r.Context(), filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"customers": mapUsers(items), "total": total})
}

type customerActionBody struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func (h *AdminHandler) CustomerAction(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body customerActionBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.RestrictCustomer(r.Context(), chi.URLParam(r, "id"), body.Kind, body.Reason, adminID, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *AdminHandler) ListActivityLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, total, err := h.Svc.ListActivityLogs(r.Context(), repository.AdminLogFilter{
		AdminID: q.Get("adminId"), TargetType: q.Get("targetType"), TargetID: q.Get("targetId"),
		Action: q.Get("action"), Limit: queryInt(q.Get("limit"), 30), Skip: queryInt(q.Get("skip"), 0),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"logs": mapLogs(items), "total": total})
}

func mapUserAdmin(u *models.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"id": u.ID, "name": u.Name, "email": u.Email, "emailVerified": u.EmailVerified,
		"role": string(u.Role), "status": string(u.Status),
		"createdAt": u.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": u.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func mapUsers(users []*models.User) []map[string]any {
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, mapUserAdmin(u))
	}
	return out
}

func mapSellerListItem(it *repository.SellerListItem) map[string]any {
	m := mapUserAdmin(it.User)
	m["storeSlug"] = it.StoreSlug
	m["storeName"] = it.StoreName
	m["totalProducts"] = it.TotalProducts
	m["publishedProducts"] = it.PublishedProducts
	m["totalOrders"] = it.TotalOrders
	m["deliveredOrders"] = it.DeliveredOrders
	m["plan"] = it.Plan
	m["isPromotionalPlan"] = it.IsPromotionalPlan
	m["verificationStatus"] = it.VerificationStatus
	if it.PlanExpiresAt != nil {
		m["planExpiresAt"] = it.PlanExpiresAt.UTC().Format(time.RFC3339)
	}
	return m
}

// mapSlugChange omits changedAt for legacy rows whose date was never recorded.
func mapSlugChange(c *models.StoreSlugChange) map[string]any {
	m := map[string]any{
		"id": c.ID, "sellerId": c.SellerID,
		"oldSlug": c.OldSlug, "newSlug": c.NewSlug, "source": c.Source,
	}
	if c.StoreName != "" {
		m["storeName"] = c.StoreName
	}
	if c.ChangedAt != nil && !c.ChangedAt.IsZero() {
		m["changedAt"] = c.ChangedAt.UTC().Format(time.RFC3339)
	}
	return m
}

func mapSlugChanges(items []*models.StoreSlugChange) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, c := range items {
		out = append(out, mapSlugChange(c))
	}
	return out
}

func mapRestrictions(items []*models.SellerRestriction) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m := map[string]any{
			"id": it.ID, "sellerId": it.SellerID, "kind": it.Kind, "reason": it.Reason,
			"adminId": it.AdminID, "startsAt": it.StartsAt.UTC().Format(time.RFC3339),
			"active": it.Active, "createdAt": it.CreatedAt.UTC().Format(time.RFC3339),
		}
		if it.EndsAt != nil {
			m["endsAt"] = it.EndsAt.UTC().Format(time.RFC3339)
		}
		out = append(out, m)
	}
	return out
}

func mapNote(n *models.AdminNote) map[string]any {
	if n == nil {
		return nil
	}
	return map[string]any{
		"id": n.ID, "targetType": n.TargetType, "targetId": n.TargetID,
		"adminId": n.AdminID, "body": n.Body,
		"createdAt": n.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": n.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func mapNotes(items []*models.AdminNote) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, n := range items {
		out = append(out, mapNote(n))
	}
	return out
}

func mapOverride(o *models.SubscriptionOverride) map[string]any {
	if o == nil {
		return nil
	}
	return map[string]any{
		"id": o.ID, "sellerId": o.SellerID, "plan": o.Plan,
		"startsAt": o.StartsAt.UTC().Format(time.RFC3339), "endsAt": o.EndsAt.UTC().Format(time.RFC3339),
		"discountPct": o.DiscountPct, "discountFixed": o.DiscountFixed,
		"isPromotional": o.IsPromotional, "reason": o.Reason, "adminId": o.AdminID,
		"active": o.Active, "createdAt": o.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func mapOverrides(items []*models.SubscriptionOverride) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, o := range items {
		out = append(out, mapOverride(o))
	}
	return out
}

func mapPromotion(p *models.SellerPromotion) map[string]any {
	if p == nil {
		return nil
	}
	m := map[string]any{
		"id": p.ID, "sellerId": p.SellerID, "kind": p.Kind, "priority": p.Priority,
		"adminId": p.AdminID, "reason": p.Reason, "active": p.Active,
		"startsAt":  p.StartsAt.UTC().Format(time.RFC3339),
		"createdAt": p.CreatedAt.UTC().Format(time.RFC3339),
	}
	if p.EndsAt != nil {
		m["endsAt"] = p.EndsAt.UTC().Format(time.RFC3339)
	}
	return m
}

func mapPromotions(items []*models.SellerPromotion) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, p := range items {
		out = append(out, mapPromotion(p))
	}
	return out
}

func mapReport(r *models.SellerReport) map[string]any {
	if r == nil {
		return nil
	}
	m := map[string]any{
		"id": r.ID, "reporterId": r.ReporterID, "reporterName": r.ReporterName,
		"reporterEmail": r.ReporterEmail, "reporterPhone": r.ReporterPhone,
		"sellerId": r.SellerID, "productId": r.ProductID, "category": r.Category,
		"description": r.Description, "evidenceUrls": r.EvidenceURLs, "status": r.Status,
		"assignedTo": r.AssignedTo, "resolution": r.Resolution, "resolvedBy": r.ResolvedBy,
		"createdAt": r.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.ResolvedAt != nil {
		m["resolvedAt"] = r.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return m
}

func mapReports(items []*models.SellerReport) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, r := range items {
		out = append(out, mapReport(r))
	}
	return out
}

func mapVerification(v *models.SellerVerification) map[string]any {
	if v == nil {
		return nil
	}
	m := map[string]any{
		"id": v.ID, "sellerId": v.SellerID, "status": v.Status, "fullName": v.FullName,
		"businessName": v.BusinessName, "businessType": v.BusinessType, "phone": v.Phone,
		"phoneVerified": v.PhoneVerified, "documentUrls": v.DocumentURLs, "notes": v.Notes,
		"rejectionReason": v.RejectionReason, "reviewedBy": v.ReviewedBy,
		"submittedAt": v.SubmittedAt.UTC().Format(time.RFC3339),
		"createdAt":   v.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":   v.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if v.ReviewedAt != nil {
		m["reviewedAt"] = v.ReviewedAt.UTC().Format(time.RFC3339)
	}
	return m
}

func mapVerifications(items []*models.SellerVerification) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, v := range items {
		out = append(out, mapVerification(v))
	}
	return out
}

func mapLog(l *models.AdminActivityLog) map[string]any {
	if l == nil {
		return nil
	}
	return map[string]any{
		"id": l.ID, "adminId": l.AdminID, "adminRole": l.AdminRole, "action": l.Action,
		"targetType": l.TargetType, "targetId": l.TargetID,
		"previousValue": l.PreviousValue, "newValue": l.NewValue,
		"reason": l.Reason, "ip": l.IP, "createdAt": l.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func mapLogs(items []*models.AdminActivityLog) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, l := range items {
		out = append(out, mapLog(l))
	}
	return out
}
