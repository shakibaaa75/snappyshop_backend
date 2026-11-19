package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/service"
)

type qProducts struct {
	repository.ProductRepository
}

func (qProducts) FindByID(_ context.Context, sellerID, id string) (*models.Product, error) {
	if sellerID == "sellerA" && id == "p1" {
		return &models.Product{ID: "p1", SellerID: "sellerA", Name: "Tee", Price: 500, Stock: 9, IsActive: true}, nil
	}
	return nil, repository.ErrNotFound
}

type qDesigns struct {
	repository.StoreDesignRepository
}

func (qDesigns) GetBySeller(context.Context, string) (*models.StoreDesign, error) {
	return &models.StoreDesign{Draft: map[string]any{"freeDeliveryThreshold": float64(1000)}}, nil
}

func (qDesigns) GetPublishedBySlug(_ context.Context, slug string) (*models.StoreDesign, error) {
	if slug == "shop" {
		return &models.StoreDesign{SellerID: "sellerA"}, nil
	}
	return nil, repository.ErrNotFound
}

type qSettings struct {
	repository.DeliverySettingsRepository
}

func (qSettings) Get(context.Context, string) (*models.DeliverySettings, error) {
	return &models.DeliverySettings{
		SellerID: "sellerA",
		Global:   models.DeliveryCharges{InsideDhaka: 60, OutsideDhaka: 120},
	}, nil
}

func quoteRouter() http.Handler {
	products, designs := qProducts{}, qDesigns{}
	svc := service.NewOrderService(nil, products, designs)
	svc.SetDeliveryQuoter(service.NewDeliveryService(qSettings{}, products, designs))
	h := &OrderHandler{Svc: svc}
	r := chi.NewRouter()
	r.Post("/store/{slug}/delivery-quote", h.Quote)
	return r
}

func postQuote(t *testing.T, slug, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/store/"+slug+"/delivery-quote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	quoteRouter().ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, out
}

func TestQuoteEndpoint_PricesByDistrict(t *testing.T) {
	cases := []struct {
		city        string
		qty         int
		zone        string
		delivery    float64
		total       float64
		freeApplied bool
	}{
		{"Dhaka", 1, "inside_dhaka", 60, 560, false},
		{"Gazipur", 1, "outside_dhaka", 120, 620, false},
		{"Madaripur", 1, "outside_dhaka", 120, 620, false},
		{"Gazipur", 2, "outside_dhaka", 0, 1000, true}, // 1000 reaches "free over 1000"
	}
	for _, c := range cases {
		body := `{"shippingCity":"` + c.city + `","lines":[{"productId":"p1","qty":` + string(rune('0'+c.qty)) + `}]}`
		code, out := postQuote(t, "shop", body)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d, body %v", c.city, code, out)
		}
		if out["deliveryZone"] != c.zone || out["delivery"] != c.delivery || out["total"] != c.total ||
			out["freeDeliveryApplied"] != c.freeApplied || out["discount"] != float64(0) {
			t.Fatalf("%s x%d: got %v", c.city, c.qty, out)
		}
	}
}

func TestQuoteEndpoint_AcceptsTheFullOrderPayload(t *testing.T) {
	// Checkout can post what it would post to place the order.
	body := `{"customerName":"Rahim","customerPhone":"01712345678","paymentMethod":"cod",
		"shippingAddress":"House 4","shippingCity":"Dhaka","lines":[{"productId":"p1","qty":1}]}`
	code, out := postQuote(t, "shop", body)
	if code != http.StatusOK || out["deliveryZone"] != "inside_dhaka" {
		t.Fatalf("status %d, body %v", code, out)
	}
}

func TestQuoteEndpoint_Errors(t *testing.T) {
	if code, _ := postQuote(t, "no-such-store", `{"shippingCity":"Dhaka","lines":[{"productId":"p1","qty":1}]}`); code != http.StatusNotFound {
		t.Fatalf("unknown store: status %d, want 404", code)
	}
	if code, _ := postQuote(t, "shop", `{"shippingCity":"Dhaka","lines":[]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty cart: status %d, want 422", code)
	}
	if code, _ := postQuote(t, "shop", `{"lines":[{"productId":"p1","qty":1}]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("no district: status %d, want 422", code)
	}
	if code, _ := postQuote(t, "shop", `{"shippingCity":"Dhaka","lines":[{"productId":"other","qty":1}]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown product: status %d, want 422", code)
	}
	// Wrong content type is refused like on every other JSON endpoint.
	req := httptest.NewRequest(http.MethodPost, "/store/shop/delivery-quote", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	quoteRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("no content type: status %d, want 415", rec.Code)
	}
}

func TestOrderDTO_CarriesZoneBreakdownAndDiscount(t *testing.T) {
	now := time.Now()
	o := &models.Order{
		ID: "o1", Number: "SNAPPY-1", Subtotal: 1000, Delivery: 0, Discount: 25, Total: 975,
		DeliveryZone: models.ZoneOutsideDhaka, DeliveryBase: 120, DeliveryWaived: true,
		CreatedAt: now, UpdatedAt: now,
	}
	raw, _ := json.Marshal(toOrderDTO(o))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["deliveryZone"] != "outside_dhaka" || m["deliveryBase"] != float64(120) ||
		m["deliveryWaived"] != true || m["discount"] != float64(25) || m["delivery"] != float64(0) || m["total"] != float64(975) {
		t.Fatalf("dto = %s", raw)
	}

	// An order placed before Delivery Settings existed: no zone key at all,
	// and the money fields are still present and numeric.
	old, _ := json.Marshal(toOrderDTO(&models.Order{ID: "o0", Subtotal: 500, Delivery: 60, Total: 560, CreatedAt: now, UpdatedAt: now}))
	var om map[string]any
	_ = json.Unmarshal(old, &om)
	if _, has := om["deliveryZone"]; has {
		t.Fatalf("old order must not invent a zone: %s", old)
	}
	if om["discount"] != float64(0) || om["deliveryWaived"] != false {
		t.Fatalf("old order dto = %s", old)
	}
}

func TestBuyerOrderDetailJSON_CarriesZoneBreakdownAndDiscount(t *testing.T) {
	d := &service.BuyerOrderDetail{
		Subtotal: 1000, Delivery: 120, Discount: 20,
		DeliveryZone: models.ZoneOutsideDhaka, DeliveryBase: 120,
	}
	raw, _ := json.Marshal(toDetailJSON(d))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["deliveryZone"] != "outside_dhaka" || m["deliveryBase"] != float64(120) ||
		m["deliveryWaived"] != false || m["discount"] != float64(20) || m["delivery"] != float64(120) {
		t.Fatalf("buyer detail json = %s", raw)
	}
}
