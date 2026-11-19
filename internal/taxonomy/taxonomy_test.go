package taxonomy

import "testing"

func TestResolveText(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	cases := map[string]string{
		"Hoodie":                "fashion.hoodie",
		"Hodie":                 "fashion.hoodie", // missing letter
		"Hoodei":                "fashion.hoodie", // swapped letters
		"hoodies":               "fashion.hoodie", // plural
		"Mens Hoodies":          "fashion.hoodie",
		"cotton hoodie for men": "fashion.hoodie",
		"T shirt":               "fashion.t-shirt",
		"tshirts":               "fashion.t-shirt",
		"T-Shirts":              "fashion.t-shirt",
		"Tshrit":                "fashion.t-shirt",
		"Shrit":                 "fashion.shirt",
		"Skirt":                 "fashion.skirt", // real category, not "shirt"
		"Kurta":                 "fashion.kurti",
		"Punjabi":               "fashion.panjabi",
		"Sharee":                "fashion.saree",
		"Sneekers":              "fashion.sneakers",
		"Smart wach":            "electronics.smart-watch",
		"Pet Supplies":          "", // must NOT be forced into a wrong category
		"xyz":                   "",
		"ring":                  "", // short words need an exact match
	}
	for in, want := range cases {
		m := ix.ResolveText(in, IsSub)
		got := m.NodeID
		if m.Ambiguous {
			got = ""
		}
		if got != want {
			t.Errorf("ResolveText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildFilter(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	if f := ix.BuildFilter("fashion", "fashion.hoodie"); f.SubID != "fashion.hoodie" || f.MainID != "" {
		t.Errorf("sub filter: %+v", f)
	}
	if f := ix.BuildFilter("fashion", ""); f.MainID != "fashion" {
		t.Errorf("main filter: %+v", f)
	}
	if f := ix.BuildFilter("nope", "nope.nope"); f != (Filter{}) {
		t.Errorf("unknown ids must be ignored: %+v", f)
	}
}
