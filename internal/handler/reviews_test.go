package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"shopbd/backend/internal/payment/sslcommerz"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

type reviewTestAuth struct{}

func (reviewTestAuth) AuthenticateAccess(s string) (*service.Claims, error) {
	return &service.Claims{UserID: s, Role: models.RoleBuyer}, nil
}

// Set REVIEW_TEST_MONGO_URI to a disposable MongoDB instance. Each run uses its own database.
func TestVerifiedReviewLifecycle(t *testing.T) {
	uri := os.Getenv("REVIEW_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires REVIEW_TEST_MONGO_URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	db := client.Database("review_test_" + primitive.NewObjectID().Hex())
	defer db.Drop(ctx)
	h := Reviews{DB: db, Log: slog.Default()}
	_, err = h.orders().InsertOne(ctx, bson.M{"_id": "order", "buyerId": "buyer", "sellerId": "seller", "paymentMethod": "cod", "status": "pending", "customerEmail": "private@example.com", "lines": bson.A{bson.M{"productId": "p", "name": "Shirt", "variantId": "red"}}})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(middleware.Authenticate(reviewTestAuth{}, h.Log))
	router.Put("/save", h.Save)
	router.Post("/receipt", h.Receipt)
	router.Get("/public", h.Public)
	router.Get("/mine", h.Buyer)
	router.Post("/moderate/{id}", h.Moderate)
	router.Post("/seller/{id}", h.SellerAction)
	call := func(method, path, who string, body any, want int) []byte {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: httpx.SessionCookie, Value: who})
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if out.Code != want {
			t.Fatalf("%s = %d, want %d: %s", path, out.Code, want, out.Body.String())
		}
		return out.Body.Bytes()
	}
	in := reviewInput{Order: "order", Product: "p", Variant: "red", Rating: 2, Title: "Fit", Body: "Tighter than expected."}
	call("PUT", "/save", "other", in, 404)
	call("PUT", "/save", "buyer", in, 204)
	call("PUT", "/save", "buyer", in, 409)
	page := func(path string) (int, string) {
		t.Helper()
		raw := call("GET", path, "buyer", nil, 200)
		var out struct {
			Count int
			Rows  []struct{ ID, Status string }
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if path == "/public?product=p" && (bytes.Contains(raw, []byte("private@example.com")) || bytes.Contains(raw, []byte(`"order"`))) {
			t.Fatal("private order data leaked")
		}
		if len(out.Rows) == 0 {
			return out.Count, ""
		}
		return out.Count, out.Rows[0].Status
	}
	if _, s := page("/mine?product=p"); s != "Pending Verification" {
		t.Fatal(s)
	}
	if _, err = h.orders().UpdateByID(ctx, "order", bson.M{"$set": bson.M{"status": "delivered", "paid": true}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := page("/public?product=p"); n != 0 {
		t.Fatal("seller flags published a review")
	}
	call("POST", "/receipt", "other", map[string]string{"order": "order"}, 404)
	call("POST", "/receipt", "buyer", map[string]string{"order": "order"}, 204)
	if n, s := page("/public?product=p"); n != 1 || s != "Published" {
		t.Fatal(n, s)
	}
	if err = h.refreshProduct(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	var row struct {
		ID string `bson:"_id"`
	}
	if err := h.reviews().FindOne(ctx, bson.M{}).Decode(&row); err != nil {
		t.Fatal(err)
	}
	call("POST", "/seller/"+row.ID, "otherSeller", reviewAction{Action: "reply", Text: "unauthorized", Revision: 1}, 404)
	call("POST", "/seller/"+row.ID, "seller", reviewAction{Action: "reply", Text: "Thanks for the sizing detail.", Revision: 1}, 204)
	in.Revision = 2
	call("PUT", "/save", "buyer", in, 204)
	if _, s := page("/mine?product=p"); s != "Pending Moderation" {
		t.Fatal(s)
	}
	call("POST", "/moderate/"+row.ID, "admin", reviewAction{Action: "approve", Revision: 3}, 204)
	if n, _ := page("/public?product=p"); n != 1 {
		t.Fatal(n)
	}
	call("POST", "/moderate/"+row.ID, "admin", reviewAction{Action: "refund", Text: "refund receipt", Revision: 4}, 204)
	if n, _ := page("/public?product=p"); n != 0 {
		t.Fatal(n)
	}
	if _, s := page("/mine?product=p"); s != "Unpublished" {
		t.Fatal(s)
	}
	// An online payment needs independent validation even with seller and buyer confirmations.
	_, err = h.orders().UpdateByID(ctx, "order", bson.M{"$unset": bson.M{"reviewBlock": ""}, "$set": bson.M{"paymentMethod": "card", "total": 750, "number": "SNAPPY-20261121-000001"}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := page("/public?product=p"); n != 0 {
		t.Fatal("online payment published without gateway evidence")
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		val := r.URL.Query().Get("val_id")
		amount := "750.00"
		if val == "wrong" {
			amount = "75.00"
		}
		json.NewEncoder(w).Encode(map[string]string{"tran_id": "SNAPPY-20261121-000001", "status": "VALID", "amount": amount, "currency": "BDT", "risk_level": "0", "val_id": val})
	}))
	defer gateway.Close()
	h.Gateway = sslcommerz.New(sslcommerz.Config{StoreID: "test", StorePassword: "test", BaseURL: gateway.URL})
	for _, tc := range []struct {
		val  string
		want int
	}{{"wrong", 400}, {"right", 204}} {
		req := httptest.NewRequest("POST", "/ipn", strings.NewReader("tran_id=SNAPPY-20261121-000001&val_id="+tc.val))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.OrderPaymentIPN(w, req)
		if w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if n, _ := page("/public?product=p"); n != 1 {
		t.Fatal("matching server-verified payment missing")
	}

}

func TestReviewPhotoValidation(t *testing.T) {
	if _, err := sanitizePhotos([]string{"data:image/png;base64,Zm9v"}); err == nil {
		t.Fatal("accepted non-image")
	}
	var src bytes.Buffer
	if err := png.Encode(&src, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	out, err := sanitizePhotos([]string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(src.Bytes())})
	if err != nil || len(out) != 1 || !strings.HasPrefix(out[0], "data:image/jpeg;base64,") {
		t.Fatal("photo was not re-encoded", err)
	}
}
