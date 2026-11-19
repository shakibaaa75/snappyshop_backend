package mongorepo

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"

	"shopbd/backend/internal/models"
)

func docVal(d bson.D, key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// A line without a variant must only be able to take stock from a product that
// has no variants; otherwise the product total drops while every variant's own
// stock stays untouched.
func TestDecrementWithoutVariantOnlyMatchesProductsWithoutVariants(t *testing.T) {
	filter, _ := decrementOp("s1", models.OrderLine{ProductID: "p1", Qty: 1})
	v, ok := docVal(filter, "combinations.0")
	if !ok {
		t.Fatal("filter must require that the product has no variants")
	}
	cond, _ := v.(bson.D)
	if len(cond) != 1 || cond[0].Key != "$exists" || cond[0].Value != false {
		t.Fatalf("want {combinations.0: {$exists: false}}, got %v", v)
	}
}

func TestDecrementWithVariantTargetsThatVariantsStock(t *testing.T) {
	filter, update := decrementOp("s1", models.OrderLine{ProductID: "p1", VariantID: "v1", Qty: 2})
	if _, ok := docVal(filter, "combinations.0"); ok {
		t.Fatal("a variant line must not be filtered to variant-less products")
	}
	v, ok := docVal(filter, "combinations")
	if !ok {
		t.Fatal("variant line must match on the variant")
	}
	em, _ := v.(bson.D)
	if len(em) != 1 || em[0].Key != "$elemMatch" {
		t.Fatalf("want an $elemMatch on the variant, got %v", v)
	}
	inc, _ := docVal(update, "$inc")
	d, _ := inc.(bson.D)
	if _, ok := docVal(d, "combinations.$.stock"); !ok {
		t.Fatalf("variant stock must be decremented, got %v", d)
	}
}
