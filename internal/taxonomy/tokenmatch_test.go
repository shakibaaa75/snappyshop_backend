package taxonomy

import (
	"fmt"
	"testing"
)

func TestTokenMatchOrderTyposAndFiller(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	cases := map[string]string{
		"Shirt Polo":                  "fashion.polo-shirt", // other word order
		"Polo Shrit":                  "fashion.polo-shirt", // typo inside a 2-word name
		"Black Polo Shirts For Men":   "fashion.polo-shirt", // colour + gender + plural are filler
		"Salwar Kamez Set":            "fashion.salwar-kameez",
		"Kameez Salwar":               "fashion.salwar-kameez",
		"Formal Shoe Men":             "fashion.formal-shoes",
		"Premium cotton hoodie combo": "fashion.hoodie",
		"Pet Supplies":                "", // shares one word with Art/Office Supplies: not enough
		"Watch Strap":                 "", // extra meaningful word: not a Watch
	}
	for in, want := range cases {
		m := ix.ResolveText(in, IsSub)
		got := m.NodeID
		if m.Ambiguous || m.Extra {
			got = ""
		}
		if got != want {
			t.Errorf("ResolveText(%q) = %q (%+v), want %q", in, got, m, want)
		}
	}
}

func TestMatchBands(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	high := []string{"Hoodie", "Hodie", "T-Shirts", "Shirt Polo", "cotton hoodie for men"}
	for _, in := range high {
		if m := ix.ResolveText(in, IsSub); m.Band != BandHigh {
			t.Errorf("%q should be BandHigh: %+v", in, m)
		}
	}
	// Two typos in one name are applied but flagged for an admin.
	ix2 := NewIndex(append(Seed(), Node{ID: "fashion.pet-wear", ParentID: "fashion", Name: "Pet Wear", IsActive: true}), nil)
	m := ix2.ResolveText("Pet Were", IsSub)
	if m.NodeID != "fashion.pet-wear" || m.Band != BandReview {
		t.Errorf("Pet Were: want pet-wear with BandReview, got %+v", m)
	}
	// Two mistakes spread over two words are NOT believed.
	if m := ix2.ResolveText("Pot Were", IsSub); m.Found() && !m.Ambiguous && m.NodeID == "fashion.pet-wear" {
		t.Errorf("Pot Were must not map to Pet Wear: %+v", m)
	}
}

func TestPlanReportsReviewAndKeepsNewCategoriesForExtras(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	p, err := Plan(ix, Input{MainID: "fashion", CustomCategory: "Salwar Kamez Set"})
	if err != nil || p.Create || p.Result.CategoryID != "fashion.salwar-kameez" {
		t.Fatalf("Salwar Kamez Set: %+v %v", p, err)
	}
	if p.Result.Band == BandNone {
		t.Errorf("band should be set: %+v", p.Result)
	}
	p, _ = Plan(ix, Input{MainID: "fashion", CustomCategory: "Watch Strap"})
	if !p.Create {
		t.Errorf("Watch Strap must create a new category: %+v", p)
	}
}

// Worst case on purpose: 4,096 three-word categories where every word is
// shared by 256 of them. (About 2 ms per lookup on a small CI machine.)
func BenchmarkResolveLargeTaxonomy(b *testing.B) {
	nodes := Seed()
	words := []string{"alpha", "bravo", "cedar", "delta", "ember", "falcon", "garnet", "harbor", "indigo", "jasper", "kestrel", "lotus", "marble", "nectar", "onyx", "pebble"}
	n := 0
	for i := range words {
		for j := range words {
			for k := range words {
				id := fmt.Sprintf("fashion.x%d", n)
				nodes = append(nodes, Node{ID: id, ParentID: "fashion", Name: words[i] + " " + words[j] + " " + words[k], IsActive: true})
				n++
			}
		}
	}
	ix := NewIndex(nodes, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.ResolveText("Falcon Garnt Marble Set", IsSub)
	}
}
