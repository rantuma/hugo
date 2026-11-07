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
	"context"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/gohugoio/hugo/common/hashing"
	"github.com/gohugoio/hugo/common/hmaps"
	"github.com/gohugoio/hugo/common/loggers"
	"github.com/gohugoio/hugo/common/para"
	"github.com/gohugoio/hugo/config"
	"github.com/gohugoio/hugo/hugolib"
	"github.com/gohugoio/hugo/resources/kinds"
	"github.com/gohugoio/hugo/resources/page"
	"github.com/spf13/cast"
)

// maxBoostCandidates is the number of top ranked search candidates to apply the exact match boosts to.
const maxBoostCandidates = 200

// maxHitsPerPage is the maximum number of search hits to return from the same page.
const maxHitsPerPage = 2

type indexedPage struct {
	site         int
	title        string
	description  string
	kind         string
	section      string
	path         string
	permalink    string
	relPermalink string
	lastmod      time.Time
	keywords     []string
	params       hmaps.Params // User defined params, e.g. from the params section in front matter.
	chunks       []chunk      // Always at least one chunk, the content before the first heading.
	contentHash  uint64
	renderErr    error
	parent       int // -1 if not set.
	children     []int
	numPages     int // The number of regular pages below a home or section page.
}

type indexedSite struct {
	title    string
	version  string
	language string
	english  bool
	home     int      // -1 if not set.
	sections []string // The top level sections.
	docs     []docRef // The search documents, one per passage.
	search   *searchIndex
}

type docRef struct {
	page, chunk, passage int
	hash                 uint64 // The hash of the passage's heading and text, 0 if it has no text.
}

// index is an immutable snapshot of the indexed content of all sites with the default role.
type index struct {
	sites       []*indexedSite
	defaultSite int
	pages       []*indexedPage
	lookup      map[string]int // Normalized URLs and paths to page.
	versions    []string
	languages   []string
}

// newIndex creates a new index from the built sites in h.
// Chunks of pages in prev with unchanged content are reused.
func newIndex(ctx context.Context, h *hugolib.HugoSites, prev *index, conv *htmltomarkdown.Converter, logger loggers.Logger) (*index, error) {
	if err := h.PrepareForOutputFormat("html"); err != nil {
		return nil, err
	}

	sites := h.Sites[0].Hugo().Sites()
	def := sites.Default()
	if def == nil {
		def = sites[0]
	}

	ix := &index{lookup: make(map[string]int)}
	var pages []page.Page
	pageIDs := make(map[page.Page]int)

	for _, s := range sites {
		if s.Role().Name() != def.Role().Name() {
			continue
		}
		si := len(ix.sites)
		if s == def {
			ix.defaultSite = si
		}
		site := &indexedSite{
			title:    s.Title(),
			version:  s.Version().Name(),
			language: s.Language().Lang,
			english:  isEnglish(s.Language().Lang),
			home:     -1,
		}
		ix.sites = append(ix.sites, site)
		if !slices.Contains(ix.versions, site.version) {
			ix.versions = append(ix.versions, site.version)
		}
		if !slices.Contains(ix.languages, site.language) {
			ix.languages = append(ix.languages, site.language)
		}

		for _, p := range s.Pages() {
			switch p.Kind() {
			case kinds.KindHome, kinds.KindSection, kinds.KindPage:
			default:
				continue
			}
			of := p.OutputFormats().Get("html")
			if of.IsZero() || of.RelPermalink() == "" {
				continue
			}
			pi := len(ix.pages)
			pageIDs[p] = pi
			pages = append(pages, p)
			ix.pages = append(ix.pages, &indexedPage{
				site:         si,
				title:        p.Title(),
				description:  p.Description(),
				kind:         p.Kind(),
				section:      p.Section(),
				path:         p.Path(),
				permalink:    of.Permalink(),
				relPermalink: of.RelPermalink(),
				lastmod:      p.Lastmod(),
				keywords:     p.Keywords(),
				params:       userParams(p),
				parent:       -1,
			})
			if p.IsHome() {
				site.home = pi
			}
			if p.Section() != "" && !slices.Contains(site.sections, p.Section()) {
				site.sections = append(site.sections, p.Section())
			}
		}
		slices.Sort(site.sections)
	}

	for pi, p := range pages {
		ip := ix.pages[pi]
		if ip.kind == kinds.KindPage {
			continue
		}
		for _, c := range p.Pages() {
			if ci, ok := pageIDs[c]; ok {
				ip.children = append(ip.children, ci)
				ix.pages[ci].parent = pi
			}
		}
		ip.numPages = len(p.RegularPagesRecursive())
	}

	if err := ix.renderChunks(ctx, h, pages, prev, conv, logger); err != nil {
		return nil, err
	}

	ix.buildLookup(pages, h.Configs.Base.WorkingDir)
	ix.buildSearch()

	return ix, nil
}

func (ix *index) renderChunks(ctx context.Context, h *hugolib.HugoSites, pages []page.Page, prev *index, conv *htmltomarkdown.Converter, logger loggers.Logger) error {
	prevPages := make(map[string]*indexedPage)
	if prev != nil {
		for _, p := range prev.pages {
			prevPages[p.permalink] = p
		}
	}
	tmpl := h.GetTemplateStore()

	r, _ := para.New(config.GetNumWorkerMultiplier()).Start(ctx)
	for pi, p := range pages {
		ip := ix.pages[pi]
		r.Run(func() error {
			v, err := p.Content(tmpl.PrepareTopLevelRenderCtx(ctx, p))
			if err != nil {
				ip.renderErr = err
				ip.chunks = emptyChunks()
				logger.Warnf("mcp: failed to render content of %q: %s", p.Path(), err)
				return nil
			}
			content := cast.ToString(v)
			ip.contentHash, _ = hashing.XXHashFromString(content)
			if pp := prevPages[ip.permalink]; pp != nil && pp.renderErr == nil && pp.contentHash == ip.contentHash {
				ip.chunks = pp.chunks
				return nil
			}
			ip.chunks, err = splitContent(conv, content, ip.permalink)
			if err != nil {
				ip.renderErr = err
				ip.chunks = emptyChunks()
				logger.Warnf("mcp: failed to convert content of %q: %s", p.Path(), err)
			}
			return nil
		})
	}
	if err := r.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}

// buildLookup maps URLs, paths, file paths and aliases to pages.
// Pages in the default site take precedence for paths that are shared between sites.
func (ix *index) buildLookup(pages []page.Page, workingDir string) {
	order := make([]int, 0, len(ix.pages))
	for pi, p := range ix.pages {
		if p.site == ix.defaultSite {
			order = append(order, pi)
		}
	}
	for pi, p := range ix.pages {
		if p.site != ix.defaultSite {
			order = append(order, pi)
		}
	}

	add := func(key string, pi int) {
		if _, found := ix.lookup[key]; !found {
			ix.lookup[key] = pi
		}
	}

	for _, pi := range order {
		ip := ix.pages[pi]
		add(normURL(ip.permalink), pi)
		add(normPath(ip.relPermalink), pi)
		add(normPath(ip.path), pi)
		if f := pages[pi].File(); f != nil && !f.IsContentAdapter() {
			add(normPath(f.Path()), pi)
			if rel, err := filepath.Rel(workingDir, f.Filename()); err == nil && !strings.HasPrefix(rel, "..") {
				add(normPath(rel), pi)
			}
		}
	}

	for _, pi := range order {
		for _, a := range pages[pi].Aliases() {
			if !strings.HasPrefix(a, "/") {
				a = path.Join(path.Dir(strings.TrimSuffix(ix.pages[pi].relPermalink, "/")), a)
			}
			add(normPath(a), pi)
		}
	}
}

func (ix *index) buildSearch() {
	docs := make([][][numFields]string, len(ix.sites))
	for pi, p := range ix.pages {
		site := ix.sites[p.site]
		meta := metaText(p)
		for ci, c := range p.chunks {
			if ci > 0 && c.Text == "" {
				// The content of a heading without text is in its subsections.
				continue
			}
			// The first passage is kept even if empty, it represents the chunk's heading
			// and, for the first chunk, the page's metadata.
			for pj, ps := range c.Passages {
				text := c.passageText(pj)
				fields := [numFields]string{
					fieldTitle:    p.title,
					fieldHeadings: strings.Join(ps.Path, " "),
					fieldBody:     text,
				}
				if ci == 0 && pj == 0 {
					fields[fieldMeta] = meta
				}
				var hash uint64
				if text != "" {
					hash, _ = hashing.XXHashFromString(ps.closestHeading() + "\x00" + text)
				}
				docs[p.site] = append(docs[p.site], fields)
				site.docs = append(site.docs, docRef{page: pi, chunk: ci, passage: pj, hash: hash})
			}
		}
	}
	for si, site := range ix.sites {
		site.search = newSearchIndex(docs[si], site.english)
	}
}

// find returns the page with the given URL or path and the URL fragment, if any.
func (ix *index) find(s string) (pi int, fragment string, found bool) {
	s, fragment, _ = strings.Cut(strings.TrimSpace(s), "#")
	s, _, _ = strings.Cut(s, "?")
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		if pi, found := ix.lookup[normURL(s)]; found {
			return pi, fragment, true
		}
		s = u.Path
	}
	key := normPath(s)
	if pi, found := ix.lookup[key]; found {
		return pi, fragment, true
	}
	for _, suffix := range []string{"/index.html", ".html"} {
		if k, ok := strings.CutSuffix(key, suffix); ok {
			if pi, found := ix.lookup[normPath(k)]; found {
				return pi, fragment, true
			}
		}
	}
	return -1, fragment, false
}

// resolveSite returns the site for the given version and language, defaulting to the default site's.
func (ix *index) resolveSite(version, language string) (*indexedSite, error) {
	def := ix.sites[ix.defaultSite]
	if version == "" {
		version = def.version
	}
	if language == "" {
		language = def.language
	}
	for _, s := range ix.sites {
		if strings.EqualFold(s.version, version) && strings.EqualFold(s.language, language) {
			return s, nil
		}
	}
	if !slices.ContainsFunc(ix.versions, func(v string) bool { return strings.EqualFold(v, version) }) {
		return nil, fmt.Errorf("unknown version %q, available versions: %s", version, strings.Join(ix.versions, ", "))
	}
	if !slices.ContainsFunc(ix.languages, func(l string) bool { return strings.EqualFold(l, language) }) {
		return nil, fmt.Errorf("unknown language %q, available languages: %s", language, strings.Join(ix.languages, ", "))
	}
	return nil, fmt.Errorf("no site for version %q and language %q", version, language)
}

type searchOptions struct {
	query    string
	version  string
	language string
	section  string
	limit    int
}

type searchHit struct {
	page, chunk, passage int
}

// search returns the best matching passages for the given options,
// at most one per anchor and maxHitsPerPage per page.
func (ix *index) search(opts searchOptions) (*indexedSite, []searchHit, error) {
	site, err := ix.resolveSite(opts.version, opts.language)
	if err != nil {
		return nil, nil, err
	}
	terms, stopWords := queryTerms(opts.query, site.english)
	if len(terms) == 0 {
		return nil, nil, fmt.Errorf("the query %q contains no searchable words", opts.query)
	}

	allow := func(int) bool { return true }
	if opts.section != "" {
		inSection := sectionMatcher(opts.section)
		if !slices.ContainsFunc(site.docs, func(d docRef) bool { return inSection(ix.pages[d.page]) }) {
			return nil, nil, fmt.Errorf("unknown section %q, top level sections: %s", opts.section, strings.Join(site.sections, ", "))
		}
		allow = func(doc int) bool { return inSection(ix.pages[site.docs[doc].page]) }
	}

	scored := site.search.search(terms, stopWords, allow)

	// Boost passages with the query terms in the page title or the headings,
	// most so the start of the page, and phrase matches.
	q := normalizeForMatch(opts.query)
	tok := tokenizer{english: site.english}
	candidates := scored[:min(len(scored), maxBoostCandidates)]
	for i, s := range candidates {
		d := site.docs[s.doc]
		p := ix.pages[d.page]
		c := &p.chunks[d.chunk]
		title := tok.coverage(p.title, terms)
		boost := 1.0
		if d.chunk == 0 && d.passage == 0 {
			boost = 1 + 2*title*title
			if tok.matchesTitle(p.title, opts.query) {
				boost *= 1.5
			}
		} else {
			ps := &c.Passages[d.passage]
			// Use the breadcrumb, e.g. Front matter > Dates for the query front matter dates.
			crumb := tok.coverage(p.title+" "+strings.Join(ps.Path, " "), terms)
			boost = (1 + 0.5*title*title) * (1 + 0.5*crumb*crumb)
			if tok.matchesTitle(ps.closestHeading(), opts.query) {
				boost *= 1.5
			}
		}
		if len(terms) > 1 && strings.Contains(normalizeForMatch(c.passageText(d.passage)), q) {
			boost *= 1.25
		}
		candidates[i].score *= boost
	}
	sortScored(candidates)

	type pageAnchor struct {
		page   int
		anchor string
	}
	var hits []searchHit
	perPage := make(map[int]int)
	seen := make(map[uint64]bool)
	seenAnchors := make(map[pageAnchor]bool)
	for _, s := range scored {
		d := site.docs[s.doc]
		pa := pageAnchor{d.page, ix.pages[d.page].chunks[d.chunk].Passages[d.passage].Anchor}
		if perPage[d.page] >= maxHitsPerPage || seen[d.hash] || seenAnchors[pa] {
			continue
		}
		if d.hash != 0 {
			// Skip identical sections in other pages, e.g. from a shared include.
			seen[d.hash] = true
		}
		seenAnchors[pa] = true
		perPage[d.page]++
		hits = append(hits, searchHit{page: d.page, chunk: d.chunk, passage: d.passage})
		if len(hits) == opts.limit {
			break
		}
	}
	return site, hits, nil
}

// sectionMatcher returns a func that reports whether a page is in the given top level section or below the given path.
func sectionMatcher(section string) func(p *indexedPage) bool {
	section = strings.Trim(strings.ToLower(strings.TrimSpace(section)), "/")
	prefix := "/" + section + "/"
	return func(p *indexedPage) bool {
		return strings.EqualFold(p.section, section) || strings.HasPrefix(strings.ToLower(p.path)+"/", prefix)
	}
}

// selection is a part of a page: a range of chunks, or a range of passages in one chunk.
type selection struct {
	start, end               int // The chunk range.
	passageStart, passageEnd int // The passage range in chunk start, if passageEnd > 0.
}

func (s selection) isPassage() bool {
	return s.passageEnd > 0
}

// selectHeading returns the section of the page with the heading with the given anchor or text.
// For h1, h2 and h3 headings, the section is the heading's chunk and its subsections,
// for h4, h5 and h6 headings and definition terms, the passages that belong to it.
// An anchor of another element below a heading selects the chunk it's in.
func (p *indexedPage) selectHeading(heading string) (sel selection, found bool) {
	heading = strings.TrimPrefix(strings.TrimSpace(heading), "#")
	if heading == "" {
		return sel, false
	}
	h := normalizeForMatch(heading)
	matchers := []struct {
		chunk   func(c *chunk) bool
		passage func(ps *passage) bool
	}{
		{
			chunk:   func(c *chunk) bool { return c.Level > 0 && strings.EqualFold(c.Anchor, heading) },
			passage: func(ps *passage) bool { return ps.Level > 0 && strings.EqualFold(ps.ID, heading) },
		},
		{
			chunk:   func(c *chunk) bool { return c.Level > 0 && normalizeForMatch(c.Heading) == h },
			passage: func(ps *passage) bool { return ps.Level > 0 && normalizeForMatch(ps.Heading) == h },
		},
		{
			chunk: func(c *chunk) bool {
				return slices.ContainsFunc(c.Anchors, func(s string) bool { return strings.EqualFold(s, heading) })
			},
		},
	}
	for _, m := range matchers {
		for ci := range p.chunks {
			c := &p.chunks[ci]
			if m.chunk(c) {
				end := ci + 1
				for end < len(p.chunks) && p.chunks[end].Level > c.Level {
					end++
				}
				return selection{start: ci, end: end}, true
			}
		}
		if m.passage == nil {
			continue
		}
		for ci := range p.chunks {
			c := &p.chunks[ci]
			for pj := range c.Passages {
				if m.passage(&c.Passages[pj]) {
					return selection{start: ci, end: ci + 1, passageStart: pj, passageEnd: c.passageRange(pj)}, true
				}
			}
		}
	}
	return sel, false
}

// anchorURL returns the URL of the element with the given id in the page.
func (p *indexedPage) anchorURL(anchor string) string {
	if anchor == "" {
		return p.permalink
	}
	return p.permalink + "#" + anchor
}

func userParams(p page.Page) hmaps.Params {
	var params hmaps.Params
	for k, v := range p.Params() {
		if p.IsRootParam(k) {
			continue
		}
		if params == nil {
			params = make(hmaps.Params)
		}
		params[k] = v
	}
	return params
}

// metaText returns the text to index for the page's metadata.
func metaText(p *indexedPage) string {
	var sb strings.Builder
	sb.WriteString(p.description)
	for _, k := range p.keywords {
		sb.WriteByte(' ')
		sb.WriteString(k)
	}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			sb.WriteByte(' ')
			sb.WriteString(v)
		case []string:
			for _, s := range v {
				walk(s)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		case hmaps.Params:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(map[string]any(p.params))
	return sb.String()
}

func isEnglish(lang string) bool {
	lang = strings.ToLower(lang)
	return lang == "en" || strings.HasPrefix(lang, "en-") || strings.HasPrefix(lang, "en_")
}

// normalizeForMatch normalizes s for case insensitive exact matching of e.g. titles.
func normalizeForMatch(s string) string {
	return strings.Join(strings.Fields(strings.TrimLeft(strings.ToLower(s), " .$")), " ")
}

func normPath(s string) string {
	s = strings.ToLower(strings.TrimSpace(filepath.ToSlash(s)))
	s = strings.TrimPrefix(s, ".")
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return path.Clean(s)
}

func normURL(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "/")
}
