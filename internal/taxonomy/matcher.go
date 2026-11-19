package taxonomy

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type MatchMethod string

const (
	MethodSelected MatchMethod = "selected" // seller picked an ID
	MethodExact    MatchMethod = "exact"
	MethodAlias    MatchMethod = "alias"
	MethodLearned  MatchMethod = "learned" // admin-approved mapping
	MethodFuzzy    MatchMethod = "fuzzy"   // typo tolerant
	MethodToken    MatchMethod = "token"   // found inside a longer phrase
	MethodNone     MatchMethod = "none"
)

// Match is the result of mapping text to one node.
type Match struct {
	NodeID    string
	Method    MatchMethod
	Score     float64 // 0..1
	Ambiguous bool    // two different categories were equally close
	// Band says how far to trust the match: high = apply automatically,
	// review = applied but worth an admin's look.
	Band MatchBand
	// Extra is true when the text has meaningful words the category does not
	// ("Watch Strap" -> Watch). Such a match is only a hint: the seller may
	// mean a different, new category.
	Extra bool
}

func (m Match) Found() bool { return m.NodeID != "" }

// NodeFilter restricts which nodes may match (e.g. only subcategories).
type NodeFilter func(Node) bool

func IsMain(n Node) bool { return n.ParentID == "" }
func IsSub(n Node) bool  { return n.ParentID != "" }

/* ---------------------------- normalization ---------------------------- */

// Normalize lowercases, drops apostrophes and turns punctuation into spaces:
// "Men's T-Shirts!" -> "mens t shirts".
func Normalize(in string) string {
	in = strings.ReplaceAll(strings.ToLower(in), "&", " and ")
	var b strings.Builder
	for _, r := range in {
		switch {
		case r == '\'' || r == '’' || r == '`':
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// singular is a tiny English singularizer. It only has to be consistent: the
// same function is applied to taxonomy names and to seller input.
func singular(w string) string {
	n := utf8.RuneCountInString(w)
	if n > 4 {
		for _, suf := range []string{"sses", "xes", "ches", "shes", "zes"} {
			if strings.HasSuffix(w, suf) {
				return w[:len(w)-2]
			}
		}
	}
	if n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") {
		return w[:len(w)-1]
	}
	return w
}

// KeyOf is a spacing-, punctuation- and plural-insensitive key:
// "T-Shirts", "t shirt" and "tshirt" all become "tshirt"; "ph" is folded to
// "f" so "headfone" == "headphone".
func KeyOf(normalized string) string {
	words := strings.Fields(normalized)
	for i, w := range words {
		words[i] = singular(w)
	}
	// "ph" sounds like "f": "Headfone" and "Headphone" are the same word to a
	// seller. Applied to taxonomy names and seller input alike.
	return strings.ReplaceAll(strings.Join(words, ""), "ph", "f")
}

var stopWords = map[string]bool{
	"for": true, "the": true, "and": true, "with": true, "of": true, "new": true, "best": true,
	"men": true, "mens": true, "man": true, "women": true, "womens": true, "woman": true,
	"ladies": true, "lady": true, "gents": true, "boys": true, "boy": true, "girls": true,
	"girl": true, "kids": true, "kid": true, "baby": true, "unisex": true, "cotton": true,
	"original": true, "premium": true, "stylish": true,
	// generic selling words and colours: they never change what the product is
	"set": true, "pack": true, "pcs": true, "piece": true, "pieces": true, "combo": true,
	"collection": true, "item": true, "items": true, "trendy": true, "latest": true,
	"hot": true, "sale": true, "offer": true, "black": true, "white": true, "red": true,
	"blue": true, "green": true, "pink": true, "yellow": true, "grey": true, "gray": true,
	"brown": true, "xl": true, "xxl": true, "large": true, "small": true, "medium": true,
}

// maxEdits is how many spelling mistakes a key of this length may have.
// Short words must match exactly: one edit turns "ring" into "king".
func maxEdits(n int) int {
	switch {
	case n <= 4:
		return 0
	case n <= 11:
		return 1
	default:
		return 2
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// editDistance is Damerau-Levenshtein (an adjacent swap counts as one edit).
// It returns limit+1 as soon as the distance is known to exceed limit.
func editDistance(a, b []rune, limit int) int {
	al, bl := len(a), len(b)
	if abs(al-bl) > limit {
		return limit + 1
	}
	prev2 := make([]int, bl+1)
	prev := make([]int, bl+1)
	for j := 0; j <= bl; j++ {
		prev[j] = j
	}
	for i := 1; i <= al; i++ {
		cur := make([]int, bl+1)
		cur[0] = i
		rowMin := i
		for j := 1; j <= bl; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if t := prev2[j-2] + 1; t < v {
					v = t
				}
			}
			cur[j] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if rowMin > limit {
			return limit + 1
		}
		prev2, prev = prev, cur
	}
	return prev[bl]
}

/* -------------------------------- index -------------------------------- */

type exactEntry struct {
	nodeID string
	via    string // "name" | "alias" | "learned"
}

type keyEntry struct {
	key    string
	runes  []rune
	nodeID string
}

// LearnedAlias is an admin-approved mapping from normalized text to a node.
type LearnedAlias struct {
	Key    string
	NodeID string
}

// Index is an in-memory lookup structure. It is safe for concurrent use.
type Index struct {
	mu    sync.RWMutex
	nodes map[string]Node
	exact map[string][]exactEntry
	keys  []keyEntry
	byLen map[int][]int // phrase-key length -> positions in keys (fuzzy lookup only scans nearby lengths)
	seen  map[string]bool
	words *wordIndex // token-level index (see tokenmatch.go)
}

// NewIndex builds an index from the given nodes (inactive ones are skipped).
func NewIndex(nodes []Node, learned []LearnedAlias) *Index {
	ix := &Index{}
	ix.Replace(nodes, learned)
	return ix
}

// Replace rebuilds the index in place and swaps it in atomically, so every
// holder of this *Index (handlers, product service) sees the change at once.
// Call it after any category is created, renamed, merged, moved or deleted.
func (ix *Index) Replace(nodes []Node, learned []LearnedAlias) {
	tmp := &Index{
		nodes: map[string]Node{},
		exact: map[string][]exactEntry{},
		byLen: map[int][]int{},
		seen:  map[string]bool{},
		words: newWordIndex(),
	}
	for _, n := range nodes {
		if n.IsActive {
			tmp.nodes[n.ID] = n
		}
	}
	for _, n := range tmp.nodes {
		tmp.addNamesLocked(n)
	}
	for _, l := range learned {
		if _, ok := tmp.nodes[l.NodeID]; ok {
			tmp.addKeyLocked(l.Key, l.NodeID, "learned")
		}
	}
	ix.mu.Lock()
	ix.nodes, ix.exact, ix.keys, ix.byLen, ix.seen, ix.words = tmp.nodes, tmp.exact, tmp.keys, tmp.byLen, tmp.seen, tmp.words
	ix.mu.Unlock()
}

func (ix *Index) addKeyLocked(key, nodeID, via string) {
	if key == "" {
		return
	}
	id := key + "\x00" + nodeID
	if ix.seen[id] {
		return
	}
	ix.seen[id] = true
	ix.exact[key] = append(ix.exact[key], exactEntry{nodeID: nodeID, via: via})
	kr := []rune(key)
	ix.byLen[len(kr)] = append(ix.byLen[len(kr)], len(ix.keys))
	ix.keys = append(ix.keys, keyEntry{key: key, runes: kr, nodeID: nodeID})
}

// addNamesLocked registers a node's name and aliases, as whole-phrase keys
// and as words for the token matcher.
func (ix *Index) addNamesLocked(n Node) {
	ix.addKeyLocked(KeyOf(Normalize(n.Name)), n.ID, "name")
	ix.words.add(n.Name, n.ID)
	for _, a := range n.Aliases {
		ix.addKeyLocked(KeyOf(Normalize(a)), n.ID, "alias")
		ix.words.add(a, n.ID)
	}
}

// AddKey registers a new alias at runtime (used when an admin approves a mapping).
func (ix *Index) AddKey(key, nodeID, via string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.addKeyLocked(key, nodeID, via)
}

// Add inserts (or replaces) one node and its lookup keys. Used the moment a
// seller's new category is created, so it is matchable and visible at once.
func (ix *Index) Add(n Node) {
	if !n.IsActive {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.nodes[n.ID] = n
	ix.addNamesLocked(n)
}

func (ix *Index) Node(id string) (Node, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n, ok := ix.nodes[id]
	return n, ok
}

func (ix *Index) list(filter NodeFilter) []Node {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]Node, 0, len(ix.nodes))
	for _, n := range ix.nodes {
		if filter(n) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (ix *Index) Mains() []Node { return ix.list(IsMain) }

func (ix *Index) Children(mainID string) []Node {
	return ix.list(func(n Node) bool { return n.ParentID == mainID })
}

func (ix *Index) exactMatch(key string, filter NodeFilter) (Match, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var hits []exactEntry
	for _, e := range ix.exact[key] {
		if filter(ix.nodes[e.nodeID]) {
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 {
		return Match{}, false
	}
	method := MethodLearned
	switch hits[0].via {
	case "name":
		method = MethodExact
	case "alias":
		method = MethodAlias
	}
	distinct := map[string]bool{}
	for _, h := range hits {
		distinct[h.nodeID] = true
	}
	return Match{NodeID: hits[0].nodeID, Method: method, Score: 1, Ambiguous: len(distinct) > 1, Band: BandHigh}, true
}

// twoMistakesInOneWord reports whether the typed words equal one phrase of
// this key, except for a single word of 4+ letters. Caller holds the read lock.
func (ix *Index) twoMistakesInOneWord(typed []string, key string) bool {
	if ix.words == nil {
		return false
	}
	for _, pos := range ix.words.byKey[key] {
		p := ix.words.phrases[pos]
		if len(p.words) != len(typed) {
			continue
		}
		diff := 0
		for i, w := range p.words {
			if w != typed[i] {
				diff++
				if len([]rune(w)) < 4 {
					return false
				}
			}
		}
		return diff == 1
	}
	return false
}

// fuzzyMatch finds the phrase key within the typo budget of key.
//
// textWords are the words the seller typed. A single word follows maxEdits.
// A multi-word name of 7+ letters may have 2 mistakes ("Pet Were" for
// "Pet Wear"): there is more text to confirm the match, and two-mistake
// matches are marked BandReview so an admin can check them.
//
// Keys are bucketed by length, so only keys of a nearby length are compared.
func (ix *Index) fuzzyMatch(key string, textWords []string, filter NodeFilter) (Match, bool) {
	kr := []rune(key)
	limit := maxEdits(len(kr))
	extended := false // 2 mistakes allowed only by the multi-word rule
	if len(textWords) > 1 && len(kr) >= 7 && limit < 2 {
		limit, extended = 2, true
	}
	if limit == 0 {
		return Match{}, false
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	best := limit + 1
	var winners []string
	for l := len(kr) - limit; l <= len(kr)+limit; l++ {
		for _, pos := range ix.byLen[l] {
			k := ix.keys[pos]
			if !filter(ix.nodes[k.nodeID]) {
				continue
			}
			d := editDistance(kr, k.runes, limit)
			if d > limit {
				continue
			}
			// Two mistakes are only believed when they sit in ONE word of 4+
			// letters and every other word is spelled right ("Pet Were" ->
			// "Pet Wear", but never "Pet Supplies" -> "Art Supplies").
			if extended && d == 2 && !ix.twoMistakesInOneWord(textWords, k.key) {
				continue
			}
			if d < best {
				best = d
				winners = []string{k.nodeID}
			} else if d == best && !containsStr(winners, k.nodeID) {
				winners = append(winners, k.nodeID)
			}
		}
	}
	if len(winners) == 0 {
		return Match{}, false
	}
	sort.Strings(winners) // deterministic: map/bucket order must never pick the winner
	n := len(kr)
	if n < 1 {
		n = 1
	}
	band := BandHigh
	if best > 1 {
		band = BandReview
	}
	return Match{
		NodeID:    winners[0],
		Method:    MethodFuzzy,
		Score:     round2(1 - float64(best)/float64(n)),
		Ambiguous: len(winners) > 1,
		Band:      band,
	}, true
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

/* ------------------------------ resolution ----------------------------- */

// ResolveText maps free text to ONE node allowed by filter.
//
// Order: whole phrase exact -> whole phrase typo -> word-by-word scoring
// (any order, typos, filler words; see tokenmatch.go).
// The Band on the result says whether to apply it silently or flag it.
func (ix *Index) ResolveText(text string, filter NodeFilter) Match {
	norm := Normalize(text)
	if norm == "" {
		return Match{Method: MethodNone}
	}

	// 1) whole phrase
	phraseKey := KeyOf(norm)
	if m, ok := ix.exactMatch(phraseKey, filter); ok {
		return m
	}
	fz, fzOK := ix.fuzzyMatch(phraseKey, phraseWords(norm), filter)
	if fzOK && !fz.Ambiguous {
		return fz
	}

	// 2) words: "Shirt Polo", "Salwar Kamez Set", "cotton hoodie for men"
	tm, tmOK := ix.tokenMatch(norm, filter)
	if tmOK && !tm.Ambiguous {
		return tm
	}
	if fzOK {
		return fz // ambiguous: caller decides
	}
	if tmOK {
		return tm // ambiguous: caller decides
	}
	return Match{Method: MethodNone}
}

// Suggestion is one "did you mean" candidate.
type Suggestion struct {
	Node  Node    `json:"node"`
	Score float64 `json:"score"`
}

// Suggest powers live suggestions while the seller types.
func (ix *Index) Suggest(q string, filter NodeFilter, limit int) []Suggestion {
	qk := KeyOf(Normalize(q))
	if qk == "" {
		return nil
	}
	qr := []rune(qk)
	qWords := phraseWords(q)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []Suggestion
	for _, n := range ix.nodes {
		if !filter(n) {
			continue
		}
		best := 0.0
		for _, raw := range append([]string{n.Name}, n.Aliases...) {
			k := KeyOf(Normalize(raw))
			s := 0.0
			switch {
			case k == qk:
				s = 1
			case strings.HasPrefix(k, qk):
				s = 0.95
			case strings.Contains(k, qk):
				s = 0.8
			default:
				kr := []rune(k)
				lim := maxEdits(len(qr))
				if lim > 0 {
					if d := editDistance(qr, kr, lim); d <= lim {
						longest := len(qr)
						if len(kr) > longest {
							longest = len(kr)
						}
						s = 1 - float64(d)/float64(longest)
					}
				}
			}
			// Same words in any order, or a longer/shorter name sharing most
			// of them: "Polo Shirts Wear" ~ "Polo Shirt".
			if ov := wordOverlap(qWords, phraseWords(raw)); ov > s {
				s = ov
			}
			if s > best {
				best = s
			}
		}
		if best > 0 {
			out = append(out, Suggestion{Node: n, Score: round2(best)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Node.Name < out[j].Node.Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
