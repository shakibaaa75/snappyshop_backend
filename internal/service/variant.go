package service

import (
	"fmt"
	"log/slog"
	"math"
	"strings"

	"shopbd/backend/internal/models"
)

// maxVariantPrice matches the ceiling for a product's own price.
const maxVariantPrice = 10_000_000

// variantSelection is what one cart line is actually buying, worked out from
// the product as it is stored now. The price and stock always come from here,
// never from the request.
type variantSelection struct {
	ID    string  // "" for a product without variants
	Label string  // e.g. "Sky Blue / M"
	Price float64 // unit price to charge
	Stock int     // units available for exactly this selection
}

// resolveVariant decides what a line buys. A non-empty problem is a message for
// the buyer and means the line must be refused.
//
// The rules:
//   - A product that has variants can only be bought as one of its own variants.
//     The base price is never used for it, not as a default and not as a
//     fallback.
//   - A product without variants cannot be bought "as a variant".
//   - The variant must be one of THIS product's variants (the product itself
//     was already looked up under the seller, so another seller's or another
//     product's variant ID matches nothing).
//   - A variant that is damaged (it does not name one value for every option,
//     its ID is not unique, or it has no price of its own) is refused rather
//     than guessed at. The operator sees a warning with the IDs to fix.
func resolveVariant(p *models.Product, variantID string) (sel variantSelection, problem string) {
	if len(p.Combinations) == 0 {
		if variantID != "" {
			return sel, fmt.Sprintf("Selected option for %s is no longer available", p.Name)
		}
		return variantSelection{Price: p.Price, Stock: p.Stock}, ""
	}
	if variantID == "" {
		return sel, fmt.Sprintf("Please choose an option for %s", p.Name)
	}

	var match *models.ProductCombination
	matches := 0
	for i := range p.Combinations {
		if p.Combinations[i].ID == variantID {
			matches++
			if match == nil {
				match = &p.Combinations[i]
			}
		}
	}
	if matches == 0 {
		return sel, fmt.Sprintf("Selected option for %s is no longer available", p.Name)
	}

	unavailable := fmt.Sprintf("%s can't be ordered in that option right now", p.Name)
	if matches > 1 {
		slog.Warn("variant is not orderable: duplicate variant id on product",
			"product", p.ID, "variant", variantID)
		return sel, unavailable
	}
	label, ok := variantLabel(p, *match)
	if !ok {
		slog.Warn("variant is not orderable: it does not name one value for every option",
			"product", p.ID, "variant", variantID)
		return sel, unavailable
	}
	if !(match.Price > 0) || math.IsInf(match.Price, 0) {
		slog.Warn("variant is not orderable: it has no price of its own",
			"product", p.ID, "variant", variantID)
		return sel, unavailable
	}
	return variantSelection{ID: match.ID, Label: label, Price: match.Price, Stock: match.Stock}, ""
}

// variantLabel builds "Sky Blue / M" and reports whether the variant is
// complete: it must name exactly one existing value for every option.
func variantLabel(p *models.Product, c models.ProductCombination) (string, bool) {
	if len(p.Options) == 0 || len(c.ValueIDs) != len(p.Options) {
		return "", false
	}
	parts := make([]string, 0, len(p.Options))
	for _, opt := range p.Options {
		valueID, ok := c.ValueIDs[opt.ID]
		if !ok {
			return "", false
		}
		label := ""
		for _, v := range opt.Values {
			if v.ID == valueID {
				label = v.Label
				break
			}
		}
		if label == "" {
			return "", false
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " / "), true
}

// validateCombinations checks the variants a seller is saving, so that what
// reaches the database is something resolveVariant can sell. It returns a
// message for the seller, or "" when everything is fine.
func validateCombinations(in ProductInput) string {
	if len(in.Combinations) == 0 {
		return ""
	}

	// The options exactly as buildOptions will keep them: nameless options and
	// valueless options are dropped, so variants must not rely on them.
	type keptOption struct {
		id     string
		values map[string]bool
	}
	var kept []keptOption
	seenOpt := map[string]bool{}
	for _, o := range in.Options {
		if strings.TrimSpace(o.Name) == "" {
			continue
		}
		values := map[string]bool{}
		count := 0
		for _, v := range o.Values {
			if strings.TrimSpace(v.Label) == "" {
				continue
			}
			count++
			if v.ID == "" {
				continue // gets a fresh ID on save, so no variant can name it
			}
			if values[v.ID] {
				return "Two values of the same option share an ID"
			}
			values[v.ID] = true
		}
		if count == 0 {
			continue
		}
		if o.ID == "" || seenOpt[o.ID] {
			return "Every option needs its own ID before variants can use it"
		}
		seenOpt[o.ID] = true
		kept = append(kept, keptOption{id: o.ID, values: values})
	}
	if len(kept) == 0 {
		return "Add at least one option (for example Size or Color) before adding variants"
	}

	seenID := map[string]bool{}
	seenSelection := map[string]bool{}
	for _, c := range in.Combinations {
		if c.ID != "" {
			if seenID[c.ID] {
				return "Variant IDs must be unique"
			}
			seenID[c.ID] = true
		}
		if len(c.ValueIDs) != len(kept) {
			return "Each variant must pick one value for every option"
		}
		picked := make([]string, len(kept))
		for i, o := range kept {
			vid, ok := c.ValueIDs[o.id]
			if !ok || !o.values[vid] {
				return "Each variant must pick one value for every option"
			}
			picked[i] = vid
		}
		key := strings.Join(picked, "\x00")
		if seenSelection[key] {
			return "Two variants have the same combination of options"
		}
		seenSelection[key] = true

		// 0 is allowed and means "same price as the product": buildCombinations
		// writes that price into the variant, so it is stored, not inherited.
		if math.IsNaN(c.Price) || c.Price < 0 || c.Price > maxVariantPrice {
			return "Variant price must be between 0 and 10,000,000"
		}
	}
	return ""
}
