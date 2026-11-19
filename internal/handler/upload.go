package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/imagesafe"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

const presignTTL = 5 * time.Minute

// PlanLookup is satisfied by *service.SubscriptionService.
type PlanLookup interface {
	PlanFor(ctx context.Context, sellerID string) (plans.Plan, error)
}

// ProductLookup is satisfied by *mongorepo.Products.
type ProductLookup interface {
	FindByID(ctx context.Context, sellerID, id string) (*models.Product, error)
}

// ObjectStore is the part of *storage.R2 the handler uses.
type ObjectStore interface {
	PutObject(ctx context.Context, key, contentType string, body []byte) error
	DeleteObject(ctx context.Context, key string) error
	PublicURL(key string) string
	PresignPut(key, contentType string, ttl time.Duration) string
}

// UploadStore is the part of *uploads.Store the handler uses.
type UploadStore interface {
	Insert(ctx context.Context, u *uploads.Upload) error
	Finish(ctx context.Context, id, status, reason string, set bson.M) error
	GetOwned(ctx context.Context, ownerID, id string) (*uploads.Upload, error)
	GetByKeyOwned(ctx context.Context, ownerID, key string) (*uploads.Upload, error)
	Delete(ctx context.Context, ownerID, id string) error
	Usage(ctx context.Context, ownerID string, since time.Time) (int64, int, int64, error)
	RejectedSince(ctx context.Context, ownerID string, since time.Time) (int64, error)
}

// UploadHandler accepts images, sanitizes them and stores them in R2.
// Browsers never write to R2: every image passes through this handler.
type UploadHandler struct {
	R2         ObjectStore
	Store      UploadStore   // required for image uploads
	Plans      PlanLookup    // optional: nil = Free quota for everyone
	Products   ProductLookup // optional: verifies productId ownership
	Scanner    uploads.Scanner
	UserLimit  *middleware.KeyedLimiter // per-user limiter (the router adds the per-IP one)
	TrustProxy bool
	Log        *slog.Logger

	mu       sync.Mutex
	inflight map[string]int
}

const (
	multipartOverhead = 256 << 10 // form fields + boundaries
	maxFieldBytes     = 256
)

// Upload — POST /api/v1/{seller|admin}/uploads  (multipart/form-data)
//
//	kind=product  [productId=...]  file=<image>
//
// The file name and the part's Content-Type are ignored. The real format is
// read from the bytes; the image is decoded and re-encoded as WebP, so only
// clean pixels are stored.
func (h *UploadHandler) Upload(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	if h.Store == nil || h.R2 == nil {
		httpx.WriteError(w, r, apperr.New(http.StatusServiceUnavailable, "UPLOADS_OFF", "Image uploads are unavailable"), h.Log)
		return
	}
	owner, ip := claims.UserID, middleware.ClientIP(r, h.TrustProxy)

	// Cheap checks first, before any body is read.
	if h.UserLimit != nil {
		if okRate, wait := h.UserLimit.Allow(owner); !okRate {
			httpx.WriteError(w, r, apperr.RateLimited(wait), h.Log)
			return
		}
	}
	if r.ContentLength > imagesafe.MaxBytes+multipartOverhead {
		httpx.WriteError(w, r, apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "File is too large (max 5 MB)"), h.Log)
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		httpx.WriteError(w, r, apperr.UnsupportedMedia("Send the image as multipart/form-data"), h.Log)
		return
	}
	if !h.acquire(owner) {
		httpx.WriteError(w, r, apperr.RateLimited(5*time.Second), h.Log)
		return
	}
	defer h.release(owner)

	// A 5 MB body on a slow phone connection can outlast the server's global
	// 15 s read timeout; give this route its own window.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
	_ = rc.SetWriteDeadline(time.Now().Add(90 * time.Second))

	// Hard cap enforced while reading, whatever Content-Length claims.
	r.Body = http.MaxBytesReader(w, r.Body, imagesafe.MaxBytes+multipartOverhead)
	fields, data, err := readUploadForm(multipart.NewReader(r.Body, params["boundary"]))
	if err != nil {
		httpx.WriteError(w, r, uploadReadError(err), h.Log)
		return
	}

	kind, ok := storage.Kinds[strings.TrimSpace(fields["kind"])]
	if !ok || !kind.IsImage() {
		httpx.WriteError(w, r, apperr.BadRequest("Unknown upload kind"), h.Log)
		return
	}
	if int64(len(data)) > kind.MaxBytes {
		httpx.WriteError(w, r, apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "File is too large (max "+humanMB(kind.MaxBytes)+")"), h.Log)
		return
	}

	ctx := r.Context()
	productID := strings.TrimSpace(fields["productId"])
	if productID != "" {
		if claims.Role != models.RoleSeller || h.Products == nil || len(productID) > 64 || !safeID(productID) {
			httpx.WriteError(w, r, apperr.BadRequest("Invalid product"), h.Log)
			return
		}
		if _, err := h.Products.FindByID(ctx, owner, productID); err != nil {
			// Not yours and not found look the same.
			httpx.WriteError(w, r, apperr.NotFound(), h.Log)
			return
		}
	}

	now := time.Now().UTC()
	if e := h.checkLimits(ctx, claims, owner, now); e != nil {
		h.audit("upload_blocked", owner, ip, "reason", e.Code)
		httpx.WriteError(w, r, e, h.Log)
		return
	}

	rec := &uploads.Upload{
		ID: randHex(16), OwnerID: owner, Role: string(claims.Role), Kind: fields["kind"], ProductID: productID,
		Status: uploads.StatusProcessing, CreatedAt: now, UpdatedAt: now,
	}
	rec.Key, err = storage.BuildKey(kind, owner, "webp")
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	if err := h.Store.Insert(ctx, rec); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}

	reject := func(reason string, e *apperr.Error) {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = h.Store.Finish(bg, rec.ID, uploads.StatusRejected, reason, nil)
		h.audit("upload_rejected", owner, ip, "reason", reason, "bytes", len(data), "sniffed", imagesafe.Sniff(data))
		httpx.WriteError(w, r, e, h.Log)
	}

	// Optional malware scan of the ORIGINAL bytes. Fails closed.
	if h.Scanner != nil {
		clean, sig, serr := h.Scanner.Scan(ctx, data)
		if serr != nil {
			h.Log.Error("upload: scanner unavailable", "error", serr.Error())
			reject("scanner_unavailable", apperr.New(http.StatusServiceUnavailable, "UPLOADS_OFF", "Image uploads are temporarily unavailable"))
			return
		}
		if !clean {
			h.audit("upload_malware", owner, ip, "signature", sig)
			reject("malware", apperr.BadRequest("This file was rejected"))
			return
		}
	}

	res, err := imagesafe.Process(ctx, data)
	if err != nil {
		switch {
		case errors.Is(err, imagesafe.ErrTooLarge):
			reject("too_large", apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", err.Error()))
		case errors.Is(err, imagesafe.ErrType):
			reject("unsupported_type", apperr.UnsupportedMedia(err.Error()))
		case errors.Is(err, imagesafe.ErrBusy):
			reject("busy", apperr.RateLimited(5*time.Second))
		case errors.Is(err, imagesafe.ErrTimeout):
			reject("timeout", apperr.BadRequest(err.Error()))
		case errors.Is(err, imagesafe.ErrCorrupt), errors.Is(err, imagesafe.ErrDimensions), errors.Is(err, imagesafe.ErrEmpty):
			reject(reasonOf(err), apperr.BadRequest(err.Error()))
		default:
			h.Log.Error("upload: process failed", "error", err.Error())
			reject("process_error", apperr.BadRequest("Image could not be processed"))
		}
		return
	}

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := h.R2.PutObject(pctx, rec.Key, "image/webp", res.WebP); err != nil {
		h.Log.Error("upload: r2 put failed", "error", err.Error(), "owner", owner)
		_ = h.Store.Finish(pctx, rec.ID, uploads.StatusRejected, "storage_error", nil)
		httpx.WriteError(w, r, apperr.New(http.StatusBadGateway, "STORAGE_ERROR", "Could not store the image, please try again"), h.Log)
		return
	}
	sum := sha256.Sum256(res.WebP)
	if err := h.Store.Finish(pctx, rec.ID, uploads.StatusApproved, "", bson.M{
		"bytes": int64(len(res.WebP)), "width": res.Width, "height": res.Height, "sha256": hex.EncodeToString(sum[:]),
	}); err != nil {
		_ = h.R2.DeleteObject(pctx, rec.Key) // never leave an unrecorded object behind
		httpx.WriteError(w, r, err, h.Log)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": rec.ID, "key": rec.Key, "publicUrl": h.R2.PublicURL(rec.Key), "status": uploads.StatusApproved,
		"width": res.Width, "height": res.Height, "bytes": len(res.WebP), "contentType": "image/webp",
	})
}

// Delete — DELETE /api/v1/{seller|admin}/uploads/{id}
// Only the owner can delete; someone else's id answers 404.
func (h *UploadHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	if h.Store == nil || h.R2 == nil {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" || len(id) > 64 || !safeID(id) {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	rec, err := h.Store.GetOwned(r.Context(), claims.UserID, id)
	if err != nil {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	h.deleteRecord(w, r, claims.UserID, rec)
}

// DeleteByKey — DELETE /api/v1/{seller|admin}/uploads?key=<object key>
// For images uploaded before records existed. The key's owner folder must be
// the caller's own; a record, when present, must also belong to the caller.
func (h *UploadHandler) DeleteByKey(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	key := r.URL.Query().Get("key")
	if h.Store == nil || h.R2 == nil || !storage.ValidKey(key) ||
		strings.Split(key, "/")[1] != storage.OwnerSegment(claims.UserID) {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	if rec, err := h.Store.GetByKeyOwned(r.Context(), claims.UserID, key); err == nil {
		h.deleteRecord(w, r, claims.UserID, rec)
		return
	}
	if err := h.R2.DeleteObject(r.Context(), key); err != nil {
		h.Log.Error("upload: delete failed", "error", err.Error(), "owner", claims.UserID)
		httpx.WriteError(w, r, apperr.New(http.StatusBadGateway, "STORAGE_ERROR", "Could not delete the image"), h.Log)
		return
	}
	h.audit("upload_deleted", claims.UserID, middleware.ClientIP(r, h.TrustProxy), "legacy", true)
	w.WriteHeader(http.StatusNoContent)
}

func (h *UploadHandler) deleteRecord(w http.ResponseWriter, r *http.Request, owner string, rec *uploads.Upload) {
	if rec.Key != "" && rec.Status == uploads.StatusApproved {
		if err := h.R2.DeleteObject(r.Context(), rec.Key); err != nil {
			h.Log.Error("upload: delete failed", "error", err.Error(), "owner", owner)
			httpx.WriteError(w, r, apperr.New(http.StatusBadGateway, "STORAGE_ERROR", "Could not delete the image"), h.Log)
			return
		}
	}
	if err := h.Store.Delete(r.Context(), owner, rec.ID); err != nil && !errors.Is(err, uploads.ErrNotFound) {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	h.audit("upload_deleted", owner, middleware.ClientIP(r, h.TrustProxy), "id", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

// checkLimits applies abuse limits and the subscription storage quota.
func (h *UploadHandler) checkLimits(ctx context.Context, c *service.Claims, owner string, now time.Time) *apperr.Error {
	rejected, err := h.Store.RejectedSince(ctx, owner, now.Add(-time.Hour))
	if err == nil && rejected >= uploads.MaxRejectedHr {
		return apperr.RateLimited(10 * time.Minute)
	}
	bytes, files, recent, err := h.Store.Usage(ctx, owner, now.Add(-24*time.Hour))
	if err != nil {
		return apperr.Internal()
	}
	if recent >= uploads.MaxPerDay {
		return apperr.RateLimited(time.Hour)
	}
	if c.Role == models.RoleSeller {
		q := uploads.QuotaFor(plans.Free)
		if h.Plans != nil {
			if p, perr := h.Plans.PlanFor(ctx, owner); perr == nil {
				q = uploads.QuotaFor(p.ID)
			}
		}
		if bytes >= q.Bytes || files >= q.Files {
			return apperr.New(http.StatusForbidden, "PLAN_LIMIT", "Image storage limit reached. Delete unused images or upgrade your plan.")
		}
	}
	return nil
}

func (h *UploadHandler) acquire(owner string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight == nil {
		h.inflight = map[string]int{}
	}
	if h.inflight[owner] >= uploads.MaxInFlight {
		return false
	}
	h.inflight[owner]++
	return true
}

func (h *UploadHandler) release(owner string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[owner]--; h.inflight[owner] <= 0 {
		delete(h.inflight, owner)
	}
}

// audit writes a security log line. Callers pass ids, counts and reasons only;
// never cookies, tokens, file names or file contents.
func (h *UploadHandler) audit(event, owner, ip string, kv ...any) {
	h.Log.Warn("security: "+event, append([]any{"owner", owner, "ip", ip}, kv...)...)
}

// readUploadForm streams the multipart body: small text fields plus exactly
// one "file" part, read with a hard size cap. Names and part content types
// supplied by the client are ignored.
func readUploadForm(mr *multipart.Reader) (map[string]string, []byte, error) {
	fields := map[string]string{}
	var data []byte
	haveFile := false
	for parts := 0; ; parts++ {
		if parts > 6 {
			return nil, nil, errBadForm
		}
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch p.FormName() {
		case "file":
			if haveFile {
				return nil, nil, errBadForm
			}
			if data, err = imagesafe.ReadLimited(p); err != nil {
				return nil, nil, err
			}
			haveFile = true
		case "kind", "productId":
			b, err := io.ReadAll(io.LimitReader(p, maxFieldBytes+1))
			if err != nil || len(b) > maxFieldBytes {
				return nil, nil, errBadForm
			}
			fields[p.FormName()] = string(b)
		default:
			return nil, nil, errBadForm
		}
	}
	if !haveFile {
		return nil, nil, imagesafe.ErrEmpty
	}
	return fields, data, nil
}

var errBadForm = errors.New("bad form")

func uploadReadError(err error) *apperr.Error {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe), errors.Is(err, imagesafe.ErrTooLarge):
		return apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "File is too large (max 5 MB)")
	case errors.Is(err, imagesafe.ErrEmpty):
		return apperr.BadRequest("File is empty")
	}
	return apperr.BadRequest("Invalid upload request")
}

func reasonOf(err error) string {
	switch {
	case errors.Is(err, imagesafe.ErrDimensions):
		return "dimensions"
	case errors.Is(err, imagesafe.ErrEmpty):
		return "empty"
	}
	return "corrupt"
}

func safeID(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return s != ""
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable")
	}
	return hex.EncodeToString(b)
}

/* ------------------------- legacy presign (video only) ------------------ */

type presignRequest struct {
	Kind        string `json:"kind"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// Presign — POST /api/v1/seller/uploads/presign
//
// Images no longer use presigned links: they must go through Upload so they
// can be validated and re-encoded. Presign remains only for banner videos,
// which this service cannot process; see the security report.
func (h *UploadHandler) Presign(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	var body presignRequest
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	kind, ok := storage.Kinds[strings.TrimSpace(body.Kind)]
	if !ok {
		httpx.WriteError(w, r, apperr.BadRequest("Unknown upload kind"), h.Log)
		return
	}
	if kind.IsImage() {
		httpx.WriteError(w, r, apperr.New(http.StatusGone, "UPLOAD_ENDPOINT_MOVED", "Images must be uploaded with POST /uploads (multipart)"), h.Log)
		return
	}
	ct := strings.ToLower(strings.TrimSpace(body.ContentType))
	ext, ok := kind.Types[ct]
	if !ok {
		httpx.WriteError(w, r, apperr.BadRequest("This file type is not allowed here"), h.Log)
		return
	}
	if body.Size <= 0 {
		httpx.WriteError(w, r, apperr.BadRequest("File is empty"), h.Log)
		return
	}
	if body.Size > kind.MaxBytes {
		httpx.WriteError(w, r, apperr.BadRequest("File is too large (max "+humanMB(kind.MaxBytes)+")"), h.Log)
		return
	}
	key, err := storage.BuildKey(kind, claims.UserID, ext)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"uploadUrl": h.R2.PresignPut(key, ct, presignTTL),
		"method":    "PUT",
		"headers":   map[string]string{"Content-Type": ct},
		"publicUrl": h.R2.PublicURL(key),
		"key":       key,
		"expiresIn": int(presignTTL.Seconds()),
	})
}

func humanMB(n int64) string { return strconv.FormatInt(n>>20, 10) + " MB" }
