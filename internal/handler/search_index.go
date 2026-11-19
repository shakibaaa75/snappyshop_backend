package handler

import (
	"strings"
	"unicode"

	"shopbd/backend/internal/models"
)

// searchDoc is the pre-tokenized searchable text of one product. It is built
// once per snapshot rebuild and is read-only afterwards.
type searchDoc struct {
	name   []string // words of the product name
	other  []string // words of category, store name and tags
	nameLC string
	descLC string
}

// tokenize splits on anything that is not a letter, digit or combining mark.
// IsMark matters for Bengali: vowel signs are marks, and without it words
// would be cut in the middle.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	})
}

func buildSearchIndex(prods []*models.Product, stores map[string]*storeInfo) []*searchDoc {
	idx := make([]*searchDoc, len(prods))
	for i, p := range prods {
		d := &searchDoc{
			name:   tokenize(p.Name),
			nameLC: strings.ToLower(p.Name),
			descLC: strings.ToLower(p.Description),
		}
		d.other = append(d.other, tokenize(p.CategoryName)...)
		if st := stores[p.SellerID]; st != nil {
			d.other = append(d.other, tokenize(st.Name)...)
		}
		for _, t := range p.Tags {
			d.other = append(d.other, tokenize(t)...)
		}
		idx[i] = d
	}
	return idx
}

// score returns 0 when the product does not match. Every typed word must
// match the START of some word (Google style), in any order:
//
//	exact word in name 8 · word prefix in name 6 · prefix in category/store/tag 2
//	typo (1 edit) in name 1 · bonus when the whole phrase is in the name
func (d *searchDoc) score(q string, qt []string) int {
	total := 0
	matched := true
	for _, t := range qt {
		best := 0
		for _, w := range d.name {
			if w == t {
				best = max(best, 8)
			} else if strings.HasPrefix(w, t) {
				best = max(best, 6)
			}
		}
		if best == 0 {
			for _, w := range d.other {
				if strings.HasPrefix(w, t) {
					best = 2
					break
				}
			}
		}
		if best == 0 && len([]rune(t)) >= 4 { // typo tolerance, longer words only
			for _, w := range d.name {
				if fuzzyPrefix(w, t) {
					best = 1
					break
				}
			}
		}
		if best == 0 {
			matched = false
			break
		}
		total += best
	}
	if matched {
		if strings.HasPrefix(d.nameLC, q) {
			total += 6
		} else if strings.Contains(d.nameLC, q) {
			total += 3
		}
		return total
	}
	// Old behaviour kept as a fallback: "pods" still finds "AirPods".
	if strings.Contains(d.nameLC, q) {
		return 3
	}
	if strings.Contains(d.descLC, q) {
		return 1
	}
	return 0
}

// fuzzyPrefix: does the typed word t equal the start of word, give or take
// one typo (one wrong, missing, extra or swapped letter)?
func fuzzyPrefix(word, t string) bool {
	w, q := []rune(word), []rune(t)
	for _, n := range []int{len(q) - 1, len(q), len(q) + 1} {
		if n > 0 && n <= len(w) && editLE1(w[:n], q) {
			return true
		}
	}
	return false
}

func editLE1(a, b []rune) bool {
	if len(a) == len(b) {
		var diff []int
		for i := range a {
			if a[i] != b[i] {
				diff = append(diff, i)
				if len(diff) > 2 {
					return false
				}
			}
		}
		switch len(diff) {
		case 0, 1:
			return true
		case 2: // swapped neighbours: "nkie" -> "nike"
			return diff[1] == diff[0]+1 && a[diff[0]] == b[diff[1]] && a[diff[1]] == b[diff[0]]
		}
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) != 1 {
		return false
	}
	i := 0
	for i < len(a) && a[i] == b[i] {
		i++
	}
	return string(a[i:]) == string(b[i+1:])
}
