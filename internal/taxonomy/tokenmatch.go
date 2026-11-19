package taxonomy

import (
	"math"
	"sort"
	"strings"
)

// Token-level matching.
//
// The whole-phrase matcher ("t shirt" -> key "tshirt") needs the seller's text
// to look like ONE taxonomy name. This stage handles everything else: words in
// a different order ("Shirt Polo"), a typo inside a longer name ("Salwar Kamez
// Set"), or extra filler words ("Black Polo Shirts For Men").
//
// How it works:
//   1. Every taxonomy name / alias is split into words and stored in an
//      inverted index: word -> phrases that contain it.
//   2. Each word the seller typed is looked up in a typo-tolerant word
//      dictionary (delete-1 index, so a lookup does not scan every word).
//   3. Only phrases that share at least one word are scored. Rare words count
//      more than common ones ("polo" > "shirt"), so cost depends on the number
//      of candidate phrases, not on the size of the taxonomy.

// MatchBand says how far a match can be trusted.
type MatchBand string

const (
	BandNone   MatchBand = ""
	BandHigh   MatchBand = "high"   // safe to apply automatically
	BandReview MatchBand = "review" // applied, but worth an admin's look
)

// Thresholds for the token stage.
const (
	minTokenScore = 0.70 // below this the text is not mapped at all
	minCoverage   = 0.85 // share of the category's words the text must contain
	highTokenBand = 0.85 // at or above this (with no extras / typos > 1) = BandHigh
	tieMargin     = 0.04 // two categories this close are "ambiguous"
	stopWeight    = 0.4  // filler words inside a category name count less
)

// phrase is one name or alias of one category, split into words.
type phrase struct {
	nodeID string
	words  []string // normalized words, in order
}

// wordIndex is the token index. It is rebuilt together with the Index.
type wordIndex struct {
	phrases []phrase
	byWord  map[string][]int           // word -> phrase positions
	byKey   map[string][]int           // words joined without spaces -> phrase positions
	nodesOf map[string]map[string]bool // word -> distinct node IDs (document frequency)
	del     map[string][]string        // word / one-letter-deleted word -> words
	seen    map[string]bool            // dedupe key of phrases already added
	long    []string                   // words long enough to allow 2 typos
}

func newWordIndex() *wordIndex {
	return &wordIndex{
		byWord:  map[string][]int{},
		byKey:   map[string][]int{},
		nodesOf: map[string]map[string]bool{},
		del:     map[string][]string{},
		seen:    map[string]bool{},
	}
}

// wordKey is the per-word version of KeyOf: singular, and "ph" sounds like "f".
func wordKey(w string) string { return strings.ReplaceAll(singular(w), "ph", "f") }

// phraseWords splits a name into normalized words.
func phraseWords(text string) []string {
	var out []string
	for _, f := range strings.Fields(Normalize(text)) {
		if k := wordKey(f); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (w *wordIndex) add(text, nodeID string) {
	words := phraseWords(text)
	if len(words) == 0 {
		return
	}
	id := nodeID + "\x00" + strings.Join(words, " ")
	if w.seen[id] {
		return
	}
	w.seen[id] = true
	pos := len(w.phrases)
	w.phrases = append(w.phrases, phrase{nodeID: nodeID, words: words})
	joined := strings.Join(words, "")
	w.byKey[joined] = append(w.byKey[joined], pos)
	for _, word := range words {
		w.byWord[word] = append(w.byWord[word], pos)
		set := w.nodesOf[word]
		if set == nil {
			set = map[string]bool{}
			w.nodesOf[word] = set
			w.addWord(word)
		}
		set[nodeID] = true
	}
}

func (w *wordIndex) addWord(word string) {
	w.del[word] = append(w.del[word], word)
	for _, d := range deletes1(word) {
		w.del[d] = append(w.del[d], word)
	}
	if len([]rune(word)) >= 12 {
		w.long = append(w.long, word)
	}
}

// deletes1 returns the word with each single letter removed.
func deletes1(word string) []string {
	r := []rune(word)
	out := make([]string, 0, len(r))
	for i := range r {
		out = append(out, string(r[:i])+string(r[i+1:]))
	}
	return out
}

type wordHit struct {
	word  string
	edits int
}

// lookup returns dictionary words within the typo budget of w (including w).
func (w *wordIndex) lookup(word string) []wordHit {
	limit := maxEdits(len([]rune(word)))
	var out []wordHit
	if _, ok := w.nodesOf[word]; ok {
		out = append(out, wordHit{word, 0})
	}
	if limit == 0 {
		return out
	}
	wr := []rune(word)
	seen := map[string]bool{word: true}
	check := func(c string) {
		if seen[c] {
			return
		}
		seen[c] = true
		if d := editDistance(wr, []rune(c), limit); d <= limit {
			out = append(out, wordHit{c, d})
		}
	}
	for _, c := range w.del[word] { // word is a deletion of c (missing letter)
		check(c)
	}
	for _, d := range deletes1(word) { // shared deletion (substitution, swap, extra letter)
		for _, c := range w.del[d] {
			check(c)
		}
	}
	if limit >= 2 { // long words may have two typos: the few long words are scanned
		for _, c := range w.long {
			check(c)
		}
	}
	return out
}

// weight is how much a word proves: rare words more than common ones.
func (w *wordIndex) weight(word string, totalNodes int) float64 {
	df := len(w.nodesOf[word])
	if df < 1 {
		df = 1
	}
	x := math.Log(1 + float64(totalNodes)/float64(df))
	if stopWords[word] {
		x *= stopWeight
	}
	return x
}

type textTok struct {
	opt  bool               // filler ("men", "set", "black"): helps, never counts as extra
	hits map[string]float64 // dictionary word -> similarity (1 = exact)
	edit map[string]int
}

// tokenMatch maps the normalized text onto one node using word-level scoring.
func (ix *Index) tokenMatch(norm string, filter NodeFilter) (Match, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	w := ix.words
	if w == nil || len(w.phrases) == 0 {
		return Match{}, false
	}

	fields := strings.Fields(norm)
	toks := make([]textTok, len(fields))
	required := 0
	for i, f := range fields {
		k := wordKey(f)
		t := textTok{opt: stopWords[f] || stopWords[k], hits: map[string]float64{}, edit: map[string]int{}}
		n := len([]rune(k))
		for _, h := range w.lookup(k) {
			t.hits[h.word] = 1 - float64(h.edits)/float64(max(n, 1))
			t.edit[h.word] = h.edits
		}
		toks[i] = t
		if !t.opt {
			required++
		}
	}
	if required == 0 { // text is only filler words: count them all
		for i := range toks {
			toks[i].opt = false
		}
		required = len(toks)
	}

	// Candidate phrases: any phrase sharing a word with the text.
	cand := map[int]bool{}
	for _, t := range toks {
		for word := range t.hits {
			for _, p := range w.byWord[word] {
				cand[p] = true
			}
		}
	}

	type scored struct {
		nodeID string
		score  float64
		extra  bool
		edits  int
	}
	best := map[string]scored{}
	total := len(ix.nodes)
	for p := range cand {
		ph := w.phrases[p]
		if !filter(ix.nodes[ph.nodeID]) {
			continue
		}
		used := make([]bool, len(toks))
		var cover, sum float64
		var maxEdit, lastPos int = 0, -1
		inOrder, matched := true, 0
		for _, word := range ph.words {
			wt := w.weight(word, total)
			sum += wt
			bi, bs := -1, 0.0
			for i, t := range toks {
				if used[i] {
					continue
				}
				if s, ok := t.hits[word]; ok && s > bs {
					bi, bs = i, s
				}
			}
			if bi < 0 {
				continue
			}
			used[bi] = true
			cover += wt * bs
			matched++
			if toks[bi].edit[word] > maxEdit {
				maxEdit = toks[bi].edit[word]
			}
			if bi < lastPos {
				inOrder = false
			}
			lastPos = bi
		}
		if matched == 0 || sum == 0 || cover/sum < minCoverage {
			continue
		}
		usedReq := 0
		for i, t := range toks {
			if used[i] && !t.opt {
				usedReq++
			}
		}
		consumed := float64(usedReq) / float64(required)
		score := (cover / sum) * (0.75 + 0.25*consumed)
		if !inOrder {
			score *= 0.97
		}
		if cur, ok := best[ph.nodeID]; !ok || score > cur.score {
			best[ph.nodeID] = scored{ph.nodeID, score, usedReq < required, maxEdit}
		}
	}
	if len(best) == 0 {
		return Match{}, false
	}
	list := make([]scored, 0, len(best))
	for _, s := range best {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].nodeID < list[j].nodeID
	})
	top := list[0]
	if top.score < minTokenScore {
		return Match{}, false
	}
	m := Match{
		NodeID: top.nodeID, Method: MethodToken, Score: round2(top.score),
		Extra: top.extra, Band: BandReview,
	}
	if !top.extra && top.edits <= 1 && top.score >= highTokenBand {
		m.Band = BandHigh
	}
	if len(list) > 1 && list[1].score >= minTokenScore && top.score-list[1].score < tieMargin {
		m.Ambiguous = true
	}
	return m, true
}

// wordOverlap scores two names by shared words, ignoring order and tolerating
// typos: 2*shared / (words in a + words in b). Used for "did you mean" lists.
func wordOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	used := make([]bool, len(b))
	shared := 0
	for _, x := range a {
		xr := []rune(x)
		lim := maxEdits(len(xr))
		for j, y := range b {
			if used[j] {
				continue
			}
			if x == y || (lim > 0 && editDistance(xr, []rune(y), lim) <= lim) {
				used[j], shared = true, 1+shared
				break
			}
		}
	}
	return 2 * float64(shared) / float64(len(a)+len(b))
}
