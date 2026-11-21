package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/database"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/payment/sslcommerz"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/image/draw"
)

type Reviews struct {
	DB      *mongo.Database
	Gateway *sslcommerz.Client
	Log     *slog.Logger
}

func (h Reviews) orders() *mongo.Collection  { return h.DB.Collection(database.OrdersCollection) }
func (h Reviews) reviews() *mongo.Collection { return h.DB.Collection("product_reviews") }
func (h Reviews) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, mongo.ErrNoDocuments) {
		err = apperr.NotFound()
	}
	httpx.WriteError(w, r, err, h.Log)
}
func actor(r *http.Request) string { c, _ := middleware.ClaimsFrom(r.Context()); return c.UserID }
func auditEntry(r *http.Request, action, detail string) bson.M {
	return bson.M{"actor": actor(r), "action": action, "detail": detail, "at": time.Now().UTC()}
}
func (h Reviews) update(w http.ResponseWriter, r *http.Request, col *mongo.Collection, filter, changes bson.M) {
	res, err := col.UpdateOne(r.Context(), filter, changes)
	if err == nil && res.MatchedCount == 0 {
		err = apperr.NotFound()
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if col.Name() == database.OrdersCollection {
		if id, ok := filter["_id"].(string); ok {
			h.RefreshOrder(r.Context(), id)
		}
	} else if id, ok := filter["_id"].(string); ok {
		var row struct{ Product string }
		if err := h.reviews().FindOne(r.Context(), bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"product": 1})).Decode(&row); err == nil {
			h.refreshAfterChange(r.Context(), row.Product)
		}
	}
	w.WriteHeader(204)
}

type reviewInput struct {
	Order, Product, Variant, Title, Body string
	Rating                               int
	Photos                               []string
	Revision                             int64
}

func sanitizePhotos(inputs []string) ([]string, error) {
	out := []string{}
	bad := apperr.BadRequest("Use up to three JPEG or PNG images, each under 1 MB and 8 megapixels.")
	if len(inputs) > 3 {
		return nil, bad
	}
	for _, v := range inputs {
		_, raw, ok := strings.Cut(v, ",")
		if !ok {
			return nil, bad
		}
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(data) > 1024*1024 {
			return nil, bad
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || (format != "jpeg" && format != "png") || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 8000000 {
			return nil, bad
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, bad
		}
		scale := math.Min(1, 480.0/float64(max(cfg.Width, cfg.Height)))
		thumb := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(cfg.Width)*scale)), max(1, int(float64(cfg.Height)*scale))))
		draw.ApproxBiLinear.Scale(thumb, thumb.Bounds(), img, img.Bounds(), draw.Src, nil)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 76}); err != nil {
			return nil, bad
		}
		out = append(out, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(buf.Bytes()))
	}
	return out, nil
}
func (h Reviews) Save(w http.ResponseWriter, r *http.Request) {
	var in reviewInput
	if err := httpx.DecodeJSON(w, r, &in, 5<<20); err != nil {
		h.fail(w, r, err)
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	if in.Rating < 1 || in.Rating > 5 || len(in.Title) == 0 || len(in.Title) > 160 || len(in.Body) == 0 || len(in.Body) > 4000 {
		h.fail(w, r, apperr.BadRequest("Enter 1–5 stars, a title (160 characters maximum), and feedback (4000 characters maximum)."))
		return
	}
	var o models.Order
	err := h.orders().FindOne(r.Context(), bson.M{"_id": in.Order, "buyerId": actor(r), "lines": bson.M{"$elemMatch": bson.M{"productId": in.Product}}}).Decode(&o)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var line *models.OrderLine
	for i := range o.Lines {
		l := &o.Lines[i]
		if l.ProductID == in.Product && l.VariantID == in.Variant {
			line = l
			break
		}
	}
	if line == nil {
		h.fail(w, r, apperr.BadRequest("Choose the variant from your order."))
		return
	}
	photos, err := sanitizePhotos(in.Photos)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(o.ID+"\x00"+in.Product)))
	moderation := "approved"
	lower := strings.ToLower(in.Title + " " + in.Body)
	if len(photos) > 0 || strings.Contains(lower, "http") || strings.Contains(lower, "www.") || strings.Contains(lower, "t.me/") {
		moderation = "pending"
	}
	now := time.Now().UTC()
	fields := bson.M{"title": in.Title, "body": in.Body, "rating": in.Rating, "photos": photos, "variant": line.Variant, "variantId": line.VariantID, "moderation": moderation, "featured": false, "updated": now}
	if in.Revision == 0 {
		fields["_id"] = id
		fields["buyer"] = actor(r)
		fields["order"] = o.ID
		fields["product"] = in.Product
		fields["name"] = line.Name
		fields["seller"] = o.SellerID
		fields["created"] = now
		fields["revision"] = 1
		fields["audit"] = bson.A{auditEntry(r, "submitted", "")}
		_, err = h.reviews().InsertOne(r.Context(), fields)
	} else {
		// Edited content must be reviewed again after an administrator intervened.
		fields["moderation"] = "pending"
		res, e := h.reviews().UpdateOne(r.Context(), bson.M{"_id": id, "buyer": actor(r), "revision": in.Revision}, bson.M{"$set": fields, "$inc": bson.M{"revision": 1}, "$push": bson.M{"audit": auditEntry(r, "edited", "")}})
		err = e
		if err == nil && res.MatchedCount == 0 {
			err = apperr.Conflict("STALE_REVIEW", "Reload this review before editing.", "revision")
		}
	}
	if mongo.IsDuplicateKeyError(err) {
		err = apperr.Conflict("DUPLICATE_REVIEW", "This purchase already has a review. Open it to edit.", "order")
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.refreshAfterChange(r.Context(), in.Product)
	w.WriteHeader(204)
}

func (h Reviews) Purchases(w http.ResponseWriter, r *http.Request) {
	product := r.URL.Query().Get("product")
	// Project only the matching lines, keeping addresses and payment details private.
	pipeline := bson.A{bson.M{"$match": bson.M{"buyerId": actor(r), "lines.productId": product}}, bson.M{"$sort": bson.M{"createdAt": -1}}, bson.M{"$unwind": "$lines"}, bson.M{"$match": bson.M{"lines.productId": product}}, bson.M{"$project": bson.M{"_id": 0, "order": "$_id", "number": 1, "variant": "$lines.variant", "variantId": "$lines.variantId", "status": 1, "paid": 1, "method": "$paymentMethod", "received": "$reviewReceipt", "blocked": "$reviewBlock"}}}
	h.rows(w, r, h.orders(), pipeline)
}
func (h Reviews) rows(w http.ResponseWriter, r *http.Request, c *mongo.Collection, p bson.A) {
	cur, err := c.Aggregate(r.Context(), p)
	rows := []bson.M{}
	if err == nil {
		defer cur.Close(r.Context())
		err = cur.All(r.Context(), &rows)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, rows)
}
func (h Reviews) Receipt(w http.ResponseWriter, r *http.Request) {
	var in struct{ Order string }
	if err := httpx.DecodeJSON(w, r, &in, 1024); err != nil {
		h.fail(w, r, err)
		return
	}
	h.update(w, r, h.orders(), bson.M{"_id": in.Order, "buyerId": actor(r), "status": "delivered", "paid": true, "reviewBlock": bson.M{"$exists": false}, "reviewReceipt": bson.M{"$ne": true}}, bson.M{"$set": bson.M{"reviewReceipt": true, "updatedAt": time.Now().UTC()}, "$push": bson.M{"reviewEvidence": auditEntry(r, "buyer confirmed receipt and payment", "")}})
}
func eq(a, b any) bson.M                    { return bson.M{"$eq": bson.A{a, b}} }
func branch(test any, status string) bson.M { return bson.M{"case": test, "then": status} }

// Resolve eligibility against the current order on every read. A refund or cancellation
// takes effect for product ratings and featured reviews even during a worker outage.
func reviewPipeline(match bson.M) bson.A {
	verifiedPayment := bson.M{"$or": bson.A{eq("$purchase.paymentMethod", "cod"), eq("$purchase.reviewGatewayVerified", true)}}
	eligible := bson.M{"$and": bson.A{
		eq("$purchase.buyerId", "$buyer"), eq("$purchase.sellerId", "$seller"),
		bson.M{"$in": bson.A{"$product", "$purchase.lines.productId"}},
		eq("$purchase.status", "delivered"), eq("$purchase.paid", true), eq("$purchase.reviewReceipt", true),
		eq(bson.M{"$ifNull": bson.A{"$purchase.reviewBlock", ""}}, ""), verifiedPayment,
	}}
	rules := bson.A{
		branch(eq("$moderation", "rejected"), "Rejected"),
		branch(eq("$moderation", "removed"), "Unpublished"),
		branch(bson.M{"$and": bson.A{eq("$everPublished", true), bson.M{"$not": bson.A{eligible}}}}, "Unpublished"),
		branch(bson.M{"$not": bson.A{eligible}}, "Pending Verification"),
		branch(eq("$moderation", "pending"), "Pending Moderation"),
	}
	return bson.A{
		bson.M{"$match": match},
		bson.M{"$lookup": bson.M{"from": database.OrdersCollection, "localField": "order", "foreignField": "_id", "as": "purchase"}},
		bson.M{"$unwind": "$purchase"},
		bson.M{"$set": bson.M{"id": "$_id", "status": bson.M{"$switch": bson.M{"branches": rules, "default": "Published"}}}},
	}
}
func (h Reviews) Public(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("feed") != "marketplace" {
		h.list(w, r, "public")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(0, min(page, 100000))
	// Marketplace publication depends only on the purchase and moderation rules.
	// Apply pagination after verification so pending reviews cannot create gaps.
	stages := reviewPipeline(bson.M{})
	pipeline := bson.A{stages[0], bson.M{"$sort": bson.D{{Key: "created", Value: -1}, {Key: "_id", Value: 1}}}}
	pipeline = append(pipeline, stages[1:]...)
	pipeline = append(pipeline, bson.M{"$match": bson.M{"status": "Published"}}, bson.M{"$skip": int64(page) * 10}, bson.M{"$limit": 11})
	pipeline = append(pipeline, nameLookups()...)
	pipeline = append(pipeline, bson.M{"$project": publicReviewFields()})
	cur, err := h.reviews().Aggregate(r.Context(), pipeline)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer cur.Close(r.Context())
	rows := []bson.M{}
	if err = cur.All(r.Context(), &rows); err != nil {
		h.fail(w, r, err)
		return
	}
	more := len(rows) > 10
	if more {
		rows = rows[:10]
	}
	httpx.WriteJSON(w, 200, bson.M{"rows": rows, "more": more})
}
func publicReviewFields() bson.M {
	return bson.M{"_id": 0, "id": 1, "product": 1, "name": 1, "buyerName": 1, "sellerName": 1, "sellerLogo": 1, "variant": 1, "rating": 1, "title": 1, "body": 1, "photos": 1, "reply": 1, "created": 1, "status": 1}
}

// nameLookups adds the buyer's name (the person who wrote the review) and the
// seller's public shop identity (the author of the reply): shop name and
// profile picture, read from the published store design exactly like the
// marketplace does (published name, then the shop name, then the slug).
// Run it only after $limit so the extra lookups touch at most one page of
// reviews. Only these three values are projected afterwards; nothing else
// from the user or shop leaves the API.
func nameLookups() bson.A {
	first := func(path string) bson.M { return bson.M{"$arrayElemAt": bson.A{path, 0}} }
	// nonEmpty is the value when it is a non-empty string, otherwise null.
	nonEmpty := func(v any) bson.M {
		return bson.M{"$cond": bson.A{bson.M{"$gt": bson.A{bson.M{"$strLenCP": bson.M{"$ifNull": bson.A{v, ""}}}, 0}}, v, nil}}
	}
	sellerName := bson.M{"$ifNull": bson.A{nonEmpty(first("$sellerShop.published.storeName")),
		bson.M{"$ifNull": bson.A{nonEmpty(first("$sellerShop.storeName")),
			bson.M{"$ifNull": bson.A{nonEmpty(first("$sellerShop.slug")), ""}}}}}}
	sellerLogo := bson.M{"$ifNull": bson.A{nonEmpty(first("$sellerShop.published.logoUrl")), ""}}
	return bson.A{
		bson.M{"$lookup": bson.M{"from": database.UsersCollection, "localField": "buyer", "foreignField": "_id", "as": "buyerUser"}},
		bson.M{"$lookup": bson.M{"from": database.StoreDesignsCollection, "localField": "seller", "foreignField": "sellerId", "as": "sellerShop"}},
		bson.M{"$set": bson.M{"buyerName": bson.M{"$ifNull": bson.A{first("$buyerUser.name"), ""}}, "sellerName": sellerName, "sellerLogo": sellerLogo}},
	}
}
func (h Reviews) Buyer(w http.ResponseWriter, r *http.Request)  { h.list(w, r, "buyer") }
func (h Reviews) Seller(w http.ResponseWriter, r *http.Request) { h.list(w, r, "seller") }
func (h Reviews) Admin(w http.ResponseWriter, r *http.Request)  { h.list(w, r, "admin") }
func (h Reviews) list(w http.ResponseWriter, r *http.Request, scope string) {
	q := r.URL.Query()
	m := bson.M{}
	if scope == "buyer" || scope == "seller" {
		m[scope] = actor(r)
	}
	if scope == "buyer" && q.Get("order") != "" {
		m["order"] = q.Get("order")
	}
	if product := q.Get("product"); product != "" {
		m["product"] = product
	}
	if scope == "public" {
		if slug := q.Get("store"); slug != "" {
			var s models.StoreDesign
			err := h.DB.Collection(database.StoreDesignsCollection).FindOne(r.Context(), bson.M{"slug": slug, "isPublished": true}, options.FindOne().SetProjection(bson.M{"sellerId": 1})).Decode(&s)
			if err != nil {
				h.fail(w, r, err)
				return
			}
			m["seller"] = s.SellerID
			m["hiddenInShop"] = bson.M{"$ne": true}
			if ids := q.Get("ids"); ids != "" {
				selected := strings.Split(ids, ",")
				if len(selected) > 10 {
					h.fail(w, r, apperr.BadRequest("Select up to ten featured reviews."))
					return
				}
				m["_id"] = bson.M{"$in": selected}
			} else {
				m["featured"] = true
			}
		} else if m["product"] == nil {
			h.fail(w, r, apperr.BadRequest("Choose a product."))
			return
		}
	}
	p := reviewPipeline(m)
	filter := bson.M{}
	if scope == "public" {
		p = append(p, bson.M{"$match": bson.M{"status": "Published"}})
	} else if s := q.Get("status"); s != "" {
		if s == "Reported" {
			filter["reported"] = true
		} else {
			filter["status"] = s
		}
	}
	if n, _ := strconv.Atoi(q.Get("rating")); n >= 1 && n <= 5 {
		filter["rating"] = n
	}
	if q.Get("photos") == "1" {
		filter["photos.0"] = bson.M{"$exists": true}
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(0, min(page, 100000))
	projection := publicReviewFields()
	if scope != "public" {
		for _, k := range []string{"order", "variantId", "revision", "featured", "reported", "hiddenInShop"} {
			projection[k] = 1
		}
	}
	if scope == "admin" {
		for _, k := range []string{"audit", "seller", "buyer", "moderation"} {
			projection[k] = 1
		}
		projection["verification"] = bson.M{"gatewayReferences": "$purchase.reviewPaymentReferences", "delivery": "$purchase.status", "paid": "$purchase.paid", "method": "$purchase.paymentMethod", "received": "$purchase.reviewReceipt", "gateway": "$purchase.reviewGatewayVerified", "block": "$purchase.reviewBlock", "events": "$purchase.events", "evidence": "$purchase.reviewEvidence", "number": "$purchase.number", "total": "$purchase.total"}
	}
	rows := bson.A{bson.M{"$match": filter}, bson.M{"$sort": bson.D{{Key: "created", Value: -1}, {Key: "_id", Value: 1}}}, bson.M{"$skip": int64(page * 10)}, bson.M{"$limit": 10}}
	rows = append(rows, nameLookups()...)
	if scope == "buyer" {
		rows = append(rows, bson.M{"$lookup": bson.M{"from": database.StoreDesignsCollection, "localField": "seller", "foreignField": "sellerId", "as": "shop"}})
		projection["slug"] = bson.M{"$arrayElemAt": bson.A{"$shop.slug", 0}}
	}
	rows = append(rows, bson.M{"$project": projection})
	p = append(p, bson.M{"$facet": bson.M{"rows": rows, "ratings": bson.A{bson.M{"$match": bson.M{"status": "Published"}}, bson.M{"$group": bson.M{"_id": "$rating", "count": bson.M{"$sum": 1}}}}}})
	cur, err := h.reviews().Aggregate(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer cur.Close(r.Context())
	var result struct {
		Rows    []bson.M
		Ratings []struct {
			Star  int `bson:"_id"`
			Count int
		}
	}
	if cur.Next(r.Context()) {
		err = cur.Decode(&result)
	} else {
		err = cur.Err()
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dist := [5]int{}
	count, sum := 0, 0
	for _, g := range result.Ratings {
		if g.Star >= 1 && g.Star <= 5 {
			dist[g.Star-1] = g.Count
			count += g.Count
			sum += g.Count * g.Star
		}
	}
	avg := 0.0
	if count > 0 {
		avg = float64(sum) / float64(count)
	}
	httpx.WriteJSON(w, 200, bson.M{"rows": result.Rows, "count": count, "average": avg, "distribution": dist})
}

type reviewAction struct {
	Action, Text string
	Revision     int64
}

func (h Reviews) SellerAction(w http.ResponseWriter, r *http.Request) { h.action(w, r, false) }
func (h Reviews) Moderate(w http.ResponseWriter, r *http.Request)     { h.action(w, r, true) }
func (h Reviews) action(w http.ResponseWriter, r *http.Request, admin bool) {
	var in reviewAction
	if err := httpx.DecodeJSON(w, r, &in, 4096); err != nil {
		h.fail(w, r, err)
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if len(in.Text) > 1000 {
		h.fail(w, r, apperr.BadRequest("Use at most 1000 characters."))
		return
	}
	f := bson.M{"_id": chi.URLParam(r, "id"), "revision": in.Revision}
	fields := bson.M{"updated": time.Now().UTC()}
	if !admin {
		f["seller"] = actor(r)
		switch in.Action {
		case "reply":
			if in.Text == "" {
				h.fail(w, r, apperr.BadRequest("Write a reply before saving."))
				return
			}
			fields["reply"] = in.Text
		case "feature":
			p := append(reviewPipeline(f), bson.M{"$match": bson.M{"status": "Published"}}, bson.M{"$project": bson.M{"_id": 1}})
			cur, err := h.reviews().Aggregate(r.Context(), p)
			if err != nil {
				h.fail(w, r, err)
				return
			}
			defer cur.Close(r.Context())
			if !cur.Next(r.Context()) {
				h.fail(w, r, apperr.BadRequest("Only published reviews may be featured."))
				return
			}
			fields["featured"] = true
			fields["hiddenInShop"] = false
		case "unfeature":
			fields["featured"] = false
			fields["hiddenInShop"] = true
		case "report":
			if in.Text == "" {
				h.fail(w, r, apperr.BadRequest("Give a reason for reporting."))
				return
			}
			fields["reported"] = true
		default:
			h.fail(w, r, apperr.BadRequest("Unknown action."))
			return
		}
	} else {
		switch in.Action {
		case "approve", "reject", "remove":
			fields["moderation"] = map[string]string{"approve": "approved", "reject": "rejected", "remove": "removed"}[in.Action]
			fields["reported"] = false
		case "refund", "return", "verify-payment":
			var row struct{ Order string }
			if err := h.reviews().FindOne(r.Context(), f).Decode(&row); err != nil {
				h.fail(w, r, err)
				return
			}
			if in.Action == "verify-payment" {
				h.verifyPayment(w, r, row.Order, in.Text)
				return
			}
			if in.Text == "" {
				h.fail(w, r, apperr.BadRequest("Enter the refund or return reference."))
				return
			}
			h.update(w, r, h.orders(), bson.M{"_id": row.Order}, bson.M{"$set": bson.M{"reviewBlock": in.Action, "updatedAt": time.Now().UTC()}, "$push": bson.M{"reviewEvidence": auditEntry(r, in.Action, in.Text)}})
			return
		default:
			h.fail(w, r, apperr.BadRequest("Unknown moderation action."))
			return
		}
	}
	h.update(w, r, h.reviews(), f, bson.M{"$set": fields, "$inc": bson.M{"revision": 1}, "$push": bson.M{"audit": auditEntry(r, in.Action, in.Text)}})
}
func (h Reviews) verifyPayment(w http.ResponseWriter, r *http.Request, orderID, valID string) {
	var o models.Order
	err := h.orders().FindOne(r.Context(), bson.M{"_id": orderID, "paymentMethod": bson.M{"$ne": "cod"}}).Decode(&o)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if h.Gateway == nil || !h.Gateway.Configured() {
		h.fail(w, r, apperr.BadRequest("Payment gateway is unavailable."))
		return
	}
	v, err := h.Gateway.Validate(r.Context(), valID)
	if err != nil {
		h.fail(w, r, apperr.BadRequest("Gateway could not confirm payment."))
		return
	}
	amount, err := v.AmountTaka()
	if err != nil || !v.OK() || v.Risky() || (v.TranID != o.ID && v.TranID != o.Number) || v.ValID != valID || !strings.EqualFold(v.Currency, "BDT") || math.IsNaN(amount) || math.Abs(amount-o.Total) > 0.001 {
		h.fail(w, r, apperr.BadRequest("Payment does not match this order."))
		return
	}
	h.update(w, r, h.orders(), bson.M{"_id": o.ID, "status": bson.M{"$ne": "cancelled"}, "reviewBlock": bson.M{"$exists": false}}, bson.M{"$set": bson.M{"reviewGatewayVerified": true, "paid": true, "updatedAt": time.Now().UTC()}, "$unset": bson.M{"reservationExpiresAt": ""}, "$addToSet": bson.M{"reviewPaymentReferences": valID}})
}

// OrderPaymentIPN receives gateway notifications without browser credentials.
// The posted amount and status are never trusted.
func (h Reviews) OrderPaymentIPN(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, apperr.BadRequest("Invalid notification."))
		return
	}
	id, val := r.PostForm.Get("tran_id"), r.PostForm.Get("val_id")
	if id == "" || val == "" || len(id) > 128 || len(val) > 128 {
		h.fail(w, r, apperr.BadRequest("Missing payment reference."))
		return
	}
	// New account-linked orders use database-issued daily sequence numbers.
	// The shorter order number fits merchant gateways with transaction-ID limits.
	var order struct {
		ID string `bson:"_id"`
	}
	err := h.orders().FindOne(r.Context(), bson.M{"buyerId": bson.M{"$exists": true}, "$or": bson.A{bson.M{"_id": id}, bson.M{"number": id}}}, options.FindOne().SetProjection(bson.M{"_id": 1})).Decode(&order)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.verifyPayment(w, r, order.ID, val)
}

// RecordOrderEvidence also works before the customer submits their first review.
func (h Reviews) RecordOrderEvidence(w http.ResponseWriter, r *http.Request) {
	var in struct{ Order, Action, Reference string }
	if err := httpx.DecodeJSON(w, r, &in, 2048); err != nil {
		h.fail(w, r, err)
		return
	}
	in.Reference = strings.TrimSpace(in.Reference)
	if in.Reference == "" || len(in.Reference) > 500 {
		h.fail(w, r, apperr.BadRequest("Enter the settlement, refund or return reference."))
		return
	}
	filter := bson.M{"_id": in.Order, "buyerId": bson.M{"$exists": true}}
	set := bson.M{"updatedAt": time.Now().UTC()}
	switch in.Action {
	case "refund", "return":
		set["reviewBlock"] = in.Action
	case "cod-settlement":
		filter["status"] = "delivered"
		filter["paymentMethod"] = "cod"
		filter["reviewBlock"] = bson.M{"$exists": false}
		set["reviewReceipt"] = true
		set["paid"] = true
	default:
		h.fail(w, r, apperr.BadRequest("Unknown order evidence."))
		return
	}
	h.update(w, r, h.orders(), filter, bson.M{"$set": set, "$push": bson.M{"reviewEvidence": auditEntry(r, in.Action, in.Reference)}})
}
