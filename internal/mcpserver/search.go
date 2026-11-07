// Copyright 2026 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcpserver

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	fieldTitle = iota
	fieldHeadings
	fieldMeta
	fieldBody
	numFields
)

// BM25F parameters.
var (
	fieldWeights = [numFields]float64{fieldTitle: 4, fieldHeadings: 2.5, fieldMeta: 2, fieldBody: 1}
	fieldB       = [numFields]float64{fieldTitle: 0.75, fieldHeadings: 0.75, fieldMeta: 0.75, fieldBody: 0.75}
)

const bm25K1 = 1.2

type posting struct {
	doc int32
	tf  [numFields]uint16
}

// searchIndex is an in-memory BM25F index.
type searchIndex struct {
	postings map[string][]posting
	lens     [][numFields]uint32
	avgLens  [numFields]float64
}

type scoredDoc struct {
	doc   int
	score float64
}

// newSearchIndex creates a new index from docs, where each doc is a set of fields to index.
func newSearchIndex(docs [][numFields]string, english bool) *searchIndex {
	ix := &searchIndex{
		postings: make(map[string][]posting),
		lens:     make([][numFields]uint32, len(docs)),
	}
	var totals, withField [numFields]float64
	tfs := make(map[string]*[numFields]uint16)
	tok := tokenizer{english: english}
	for i, fields := range docs {
		clear(tfs)
		for f, s := range fields {
			tok.tokens(s, func(term string, _, _ int) {
				tf, ok := tfs[term]
				if !ok {
					tf = new([numFields]uint16)
					tfs[term] = tf
				}
				if tf[f] < math.MaxUint16 {
					tf[f]++
				}
				ix.lens[i][f]++
			})
			if ix.lens[i][f] > 0 {
				totals[f] += float64(ix.lens[i][f])
				withField[f]++
			}
		}
		for term, tf := range tfs {
			ix.postings[term] = append(ix.postings[term], posting{doc: int32(i), tf: *tf})
		}
	}
	for f := range totals {
		// As in Lucene, the average is over the documents with the field,
		// so sparse fields, e.g. the page metadata, aren't penalized.
		ix.avgLens[f] = 1
		if withField[f] > 0 {
			ix.avgLens[f] = max(totals[f]/withField[f], 1)
		}
	}
	return ix
}

// coverage returns the fraction of terms found in s.
func (t tokenizer) coverage(s string, terms []string) float64 {
	if s == "" || len(terms) == 0 {
		return 0
	}
	var found []string
	t.tokens(s, func(term string, _, _ int) {
		if slices.Contains(terms, term) && !slices.Contains(found, term) {
			found = append(found, term)
		}
	})
	return float64(len(found)) / float64(len(terms))
}

// matchesTitle reports whether the query q matches the title, ignoring case and plural forms,
// e.g. shortcode matches Shortcodes and where matches collections.Where.
func (t tokenizer) matchesTitle(title, q string) bool {
	title, q = t.stemWords(title), t.stemWords(q)
	return q != "" && (title == q || strings.HasSuffix(title, "."+q))
}

// stemWords returns the stemmed lower case words in s, separated by spaces.
func (t tokenizer) stemWords(s string) string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isWordRune(r) && !isJoiner(r) })
	for i, w := range words {
		words[i] = t.stem(strings.Trim(w, "._-"))
	}
	return strings.Join(words, " ")
}

// search returns the documents matching the given terms, best match first.
// The optional terms, e.g. stop words, add to the score of documents matching any of the terms.
// allow is used to filter the documents.
func (ix *searchIndex) search(terms, optional []string, allow func(doc int) bool) []scoredDoc {
	if len(terms) == 0 {
		return nil
	}
	n := float64(len(ix.lens))
	scores := make(map[int]float64)
	matches := make(map[int]int)
	for i, term := range slices.Concat(terms, optional) {
		required := i < len(terms)
		postings := ix.postings[term]
		if len(postings) == 0 {
			continue
		}
		df := float64(len(postings))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range postings {
			doc := int(p.doc)
			if !allow(doc) {
				continue
			}
			if !required {
				if _, found := scores[doc]; !found {
					continue
				}
			}
			var tf float64
			for f := range numFields {
				if p.tf[f] == 0 {
					continue
				}
				norm := 1 - fieldB[f] + fieldB[f]*float64(ix.lens[doc][f])/ix.avgLens[f]
				tf += fieldWeights[f] * float64(p.tf[f]) / norm
			}
			scores[doc] += idf * tf / (bm25K1 + tf)
			if required {
				matches[doc]++
			}
		}
	}

	result := make([]scoredDoc, 0, len(scores))
	for doc, score := range scores {
		// Prefer documents matching more of the query terms.
		coord := float64(matches[doc]) / float64(len(terms))
		result = append(result, scoredDoc{doc: doc, score: score * (0.5 + 0.5*coord*coord)})
	}
	sortScored(result)
	return result
}

func sortScored(s []scoredDoc) {
	slices.SortFunc(s, func(a, b scoredDoc) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.doc, b.doc)
	})
}

// maxQueryTerms is the maximum number of search terms used from a query.
const maxQueryTerms = 32

// queryTerms returns the unique search terms in q and the English stop words in q.
// Stop words are only used for scoring, unless q contains nothing but stop words,
// e.g. the name of the where function.
func queryTerms(q string, english bool) (terms, stopWords []string) {
	tokenizer{english: english}.tokens(q, func(term string, _, _ int) {
		switch {
		case slices.Contains(terms, term), slices.Contains(stopWords, term):
		case english && englishStopWords[term]:
			stopWords = append(stopWords, term)
		case len(terms) < maxQueryTerms:
			terms = append(terms, term)
		}
	})
	if len(terms) == 0 {
		return stopWords, nil
	}
	return terms, stopWords
}

// tokenizer splits text into lower case search terms.
//
// Identifiers joined by dots, underscores and hyphens, e.g. strings.Replace,
// markup.goldmark.renderer.unsafe and go-html-template, are emitted as a whole,
// as their suffixes of up to maxSuffixParts parts (e.g. renderer.unsafe) and as their parts.
// Mixed case identifiers, e.g. RelPermalink, are also emitted as their camel case parts.
// CJK text is split into bigrams.
type tokenizer struct {
	english bool // Whether to apply English stemming.
}

// maxSuffixParts is the maximum number of parts in the suffixes emitted for joined identifiers.
// Otherwise, the total length of the suffixes grows quadratically with the length of the identifier.
const maxSuffixParts = 8

// tokens calls fn for each term in s with its byte offsets in s.
func (t tokenizer) tokens(s string, fn func(term string, start, end int)) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case isCJK(r):
			start := i
			for i < len(s) {
				r, size := utf8.DecodeRuneInString(s[i:])
				if !isCJK(r) {
					break
				}
				i += size
			}
			t.cjk(s[start:i], start, fn)
		case isWordRune(r):
			start := i
			i += size
			for i < len(s) {
				r, size := utf8.DecodeRuneInString(s[i:])
				if isWordRune(r) && !isCJK(r) {
					i += size
					continue
				}
				if isJoiner(r) && i+size < len(s) {
					if r2, _ := utf8.DecodeRuneInString(s[i+size:]); isWordRune(r2) && !isCJK(r2) {
						i += size
						continue
					}
				}
				break
			}
			t.word(s[start:i], start, fn)
		default:
			i += size
		}
	}
}

func (t tokenizer) word(w string, start int, fn func(term string, start, end int)) {
	if !strings.ContainsFunc(w, isJoiner) {
		t.simpleWord(w, start, fn)
		return
	}
	end := start + len(w)
	fn(strings.ToLower(w), start, end)
	parts := strings.FieldsFunc(w, isJoiner)
	offset := 0
	for i, part := range parts {
		pos := strings.Index(w[offset:], part) + offset
		if n := len(parts) - i; i > 0 && n > 1 && n <= maxSuffixParts {
			fn(strings.ToLower(w[pos:]), start+pos, end)
		}
		t.simpleWord(part, start+pos, fn)
		offset = pos + len(part)
	}
}

func (t tokenizer) simpleWord(w string, start int, fn func(term string, start, end int)) {
	t.term(w, start, fn)
	if strings.ToLower(w) == w {
		return
	}
	parts := splitCamelCase(w)
	if len(parts) < 2 {
		return
	}
	pos := start
	for _, p := range parts {
		t.term(p, pos, fn)
		pos += len(p)
	}
}

func (t tokenizer) term(w string, start int, fn func(term string, start, end int)) {
	lower := strings.ToLower(w)
	if len(lower) == 1 && lower[0] >= 'a' && lower[0] <= 'z' {
		return
	}
	fn(t.stem(lower), start, start+len(w))
}

// stem is a light English stemmer that maps singular and plural forms to the same term,
// e.g. alias and aliases to alias, cache and caches to cach and taxonomy and taxonomies to taxonomi.
func (t tokenizer) stem(s string) string {
	if !t.english || len(s) < 3 {
		return s
	}
	switch {
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "ias"):
	case strings.HasSuffix(s, "ies") && len(s) > 4:
		s = s[:len(s)-3] + "i"
	case strings.HasSuffix(s, "s"):
		s = s[:len(s)-1]
	}
	if len(s) > 3 {
		switch s[len(s)-1] {
		case 'e':
			s = s[:len(s)-1]
		case 'y':
			s = s[:len(s)-1] + "i"
		}
	}
	return s
}

func (t tokenizer) cjk(s string, start int, fn func(term string, start, end int)) {
	var prev string
	prevPos := 0
	for i, r := range s {
		cur := string(r)
		if prev != "" {
			fn(prev+cur, start+prevPos, start+i+len(cur))
		} else if i+len(cur) == len(s) {
			// A single CJK character.
			fn(cur, start+i, start+i+len(cur))
		}
		prev, prevPos = cur, i
	}
}

// splitCamelCase splits e.g. RelPermalink into Rel and Permalink, and toJSON into to and JSON.
func splitCamelCase(s string) []string {
	var parts []string
	runes := []rune(s)
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		split := unicode.IsLower(prev) && unicode.IsUpper(cur) ||
			unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) ||
			unicode.IsDigit(prev) != unicode.IsDigit(cur)
		if split {
			parts = append(parts, string(runes[start:i]))
			start = i
		}
	}
	return append(parts, string(runes[start:]))
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isJoiner(r rune) bool {
	return r == '.' || r == '_' || r == '-'
}

func isCJK(r rune) bool {
	// The prolonged sound marks, e.g. in テーマ, are not in the Katakana script.
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) || r == 'ー' || r == 'ｰ'
}

// englishStopWords is a set of stemmed common English words.
var englishStopWords = func() map[string]bool {
	m := make(map[string]bool)
	for w := range strings.FieldsSeq(`an and are as at be by can do does for from how if in into is it its my of on or
so that the their then there these this to was what when where which while who why will with you your`) {
		m[tokenizer{english: true}.stem(w)] = true
	}
	return m
}()

// snippet returns an excerpt of text of at most maxLen bytes with as many of the search terms as possible.
func snippet(text string, terms []string, english bool, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	type match struct {
		term       string
		start, end int
	}
	var matches []match
	tokenizer{english: english}.tokens(text, func(term string, start, end int) {
		if slices.Contains(terms, term) {
			matches = append(matches, match{term, start, end})
		}
	})

	start := 0
	if len(matches) > 0 {
		// Find the window with the most distinct terms.
		best, bestCount := 0, 0
		seen := make(map[string]bool)
		for i := range matches {
			clear(seen)
			for j := i; j < len(matches) && matches[j].end-matches[i].start <= maxLen; j++ {
				seen[matches[j].term] = true
			}
			if len(seen) > bestCount {
				best, bestCount = i, len(seen)
			}
		}
		// Add some context before the first match.
		start = max(0, matches[best].start-maxLen/5)
	}
	end := min(len(text), start+maxLen)
	if end == len(text) {
		start = max(0, end-maxLen)
	}

	if start > 0 {
		if i := strings.IndexByte(text[start:end], ' '); i != -1 && i < maxLen/4 {
			start += i + 1
		}
		for start < end && !utf8.RuneStart(text[start]) {
			start++
		}
	}
	if end < len(text) {
		if i := strings.LastIndexByte(text[start:end], ' '); i > maxLen/2 {
			end = start + i
		}
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
	}

	s := text[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}
