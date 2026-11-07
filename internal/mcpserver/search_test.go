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
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	qt "github.com/frankban/quicktest"
)

func TestQueryTerms(t *testing.T) {
	c := qt.New(t)
	for _, test := range []struct {
		q         string
		english   bool
		terms     []string
		stopWords []string
	}{
		{"How do I use .RelPermalink?", true, []string{"use", "relpermalink", "rel", "permalink"}, []string{"how", "do"}},
		{"where", true, []string{"wher"}, nil},
		{"Taxonomies and the taxonomy", true, []string{"taxonomi"}, []string{"and", "the"}},
		{"Taxonomies", false, []string{"taxonomies"}, nil},
		{"markup.goldmark.renderer.unsafe", true, []string{
			"markup.goldmark.renderer.unsafe", "markup", "goldmark.renderer.unsafe", "goldmark", "renderer.unsafe", "renderer", "unsaf",
		}, nil},
		{"go-html-template", false, []string{"go-html-template", "go", "html-template", "html", "template"}, nil},
		{"toJSON", false, []string{"tojson", "to", "json"}, nil},
		{"Hugoのテーマ", false, []string{"hugo", "のテ", "テー", "ーマ"}, nil},
		{"語", false, []string{"語"}, nil},
		{"한국어", false, []string{"한국", "국어"}, nil},
	} {
		terms, stopWords := queryTerms(test.q, test.english)
		c.Assert(terms, qt.DeepEquals, test.terms, qt.Commentf(test.q))
		c.Assert(stopWords, qt.DeepEquals, test.stopWords, qt.Commentf(test.q))
	}
}

// Long queries must not cause a quadratic number or length of search terms.
func TestQueryTermsLimits(t *testing.T) {
	c := qt.New(t)
	var words []string
	for i := range 2 * maxQueryTerms {
		words = append(words, fmt.Sprintf("word%d", i))
	}

	terms, _ := queryTerms(strings.Join(words, " "), false)
	c.Assert(terms, qt.DeepEquals, words[:maxQueryTerms])

	parts := words[:2*maxSuffixParts]
	terms, _ = queryTerms(strings.Join(parts, "."), false)
	c.Assert(terms, qt.Contains, strings.Join(parts[len(parts)-maxSuffixParts:], "."))
	c.Assert(terms, qt.Not(qt.Contains), strings.Join(parts[len(parts)-maxSuffixParts-1:], "."))
}

func TestSnippet(t *testing.T) {
	c := qt.New(t)
	filler := strings.Repeat("Größe und Länge. ", 20)
	text := filler + "Use the where function to filter a collection. " + filler
	terms, _ := queryTerms("filter collection", true)
	s := snippet(text, terms, true, 80)
	c.Assert(s, qt.Contains, "function to filter a collection.")
	c.Assert(strings.HasPrefix(s, "…"), qt.IsTrue)
	c.Assert(strings.HasSuffix(s, "…"), qt.IsTrue)
	c.Assert(len(s) <= 80+2*len("…"), qt.IsTrue)
	c.Assert(utf8.ValidString(s), qt.IsTrue)
}
