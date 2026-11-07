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
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gohugoio/hugo/common/hmaps"
	"github.com/gohugoio/hugo/parser/metadecoders"
	"github.com/gohugoio/hugo/resources/kinds"
)

const (
	// maxPartSize is the maximum size in bytes of the content in a get_page response.
	// Longer pages are split into parts.
	maxPartSize = 30000

	// maxListEntries is the maximum number of pages or headings to list.
	maxListEntries = 200

	// maxParamsSize is the maximum size in bytes of the front matter params in a get_page response.
	maxParamsSize = 4096

	// maxDescriptionLen is the maximum length in bytes of page descriptions in lists.
	maxDescriptionLen = 200

	// snippetLen is the maximum length in bytes of search result snippets.
	snippetLen = 280
)

type pageRequest struct {
	page     int
	heading  string // Anchor or text of the heading to get. It's an error if not found.
	fragment string // URL fragment. The whole page is returned if not found.
	part     int    // 1-based.
	warning  string // Added to the header, e.g. if the content may be outdated.
}

type pageHeader struct {
	Title       string       `yaml:"title"`
	URL         string       `yaml:"url"`
	Heading     string       `yaml:"heading,omitempty"`
	Description string       `yaml:"description,omitempty"`
	Section     string       `yaml:"section,omitempty"`
	Version     string       `yaml:"version,omitempty"`
	Language    string       `yaml:"language,omitempty"`
	Lastmod     string       `yaml:"lastmod,omitempty"`
	Part        string       `yaml:"part,omitempty"`
	Error       string       `yaml:"error,omitempty"`
	Warning     string       `yaml:"warning,omitempty"`
	Params      hmaps.Params `yaml:"params,omitempty"`
}

// unit is Markdown that is kept in one part if possible.
type unit struct {
	markdown string
	chunk    int // -1 if not a chunk.
}

// renderPage renders the requested page or section of a page as Markdown with a YAML front matter header.
// Content larger than maxSize bytes is split into parts.
func (ix *index) renderPage(req pageRequest, maxSize int) (string, error) {
	p := ix.pages[req.page]
	sel := selection{end: len(p.chunks)}
	if req.heading != "" {
		var found bool
		if sel, found = p.selectHeading(req.heading); !found {
			return "", fmt.Errorf("heading %q not found in %s. %s", req.heading, p.permalink, p.headingsHint())
		}
	} else if req.fragment != "" {
		if s, found := p.selectHeading(req.fragment); found {
			sel = s
		}
	}
	first := &p.chunks[sel.start]

	var units []unit
	if sel.isPassage() {
		md := first.Markdown[first.Passages[sel.passageStart].mdStart:first.Passages[sel.passageEnd-1].mdEnd]
		units = append(units, unit{markdown: md, chunk: sel.start})
	} else {
		for ci := sel.start; ci < sel.end; ci++ {
			if md := p.chunks[ci].Markdown; md != "" {
				units = append(units, unit{markdown: md, chunk: ci})
			}
		}
		if first.Level == 0 && len(p.children) > 0 {
			units = append(units, unit{markdown: ix.childrenMarkdown(p), chunk: -1})
		}
	}

	parts, unitParts := paginate(units, maxSize)
	n := max(len(parts), 1)
	part := max(req.part, 1)
	if part > n {
		return "", fmt.Errorf("part %d does not exist, the page has %d part(s)", part, n)
	}

	site := ix.sites[p.site]
	h := pageHeader{
		Title:       p.displayTitle(),
		URL:         p.permalink,
		Description: p.description,
		Section:     p.section,
		Warning:     req.warning,
	}
	switch {
	case sel.isPassage():
		ps := first.Passages[sel.passageStart]
		h.URL = p.anchorURL(ps.Anchor)
		h.Heading = strings.Join(ps.Path, " > ")
	case first.Level > 0:
		h.URL = p.anchorURL(first.Anchor)
		h.Heading = strings.Join(first.Path, " > ")
	}
	if len(ix.versions) > 1 {
		h.Version = site.version
	}
	if len(ix.languages) > 1 {
		h.Language = site.language
	}
	if !p.lastmod.IsZero() {
		h.Lastmod = p.lastmod.Format(time.DateOnly)
	}
	if n > 1 {
		h.Part = fmt.Sprintf("%d of %d", part, n)
	}
	if p.renderErr != nil {
		h.Error = "failed to render the content: " + p.renderErr.Error()
	}
	if part == 1 {
		h.Params = p.headerParams()
	}
	b, err := metadecoders.MarshalYAML(h)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(b)
	sb.WriteString("---\n\n")
	if len(parts) == 0 {
		sb.WriteString("This page has no content.\n")
	} else {
		sb.WriteString(parts[part-1])
		sb.WriteByte('\n')
	}
	if n > 1 || sel.isPassage() {
		sb.WriteString("\n---\n\n")
	}
	if n > 1 {
		if part < n {
			fmt.Fprintf(&sb, "This is part %d of %d. Call get_page again with part=%d to continue.", part, n, part+1)
		} else {
			fmt.Fprintf(&sb, "This is the last part (%d of %d).", part, n)
		}
		if outline := outline(p, units, unitParts); part == 1 && outline != "" && !sel.isPassage() {
			sb.WriteString(" Outline with the part each section starts in:\n\n")
			sb.WriteString(outline)
		}
		sb.WriteByte('\n')
	}
	if sel.isPassage() && part == n {
		if first.Level > 0 {
			fmt.Fprintf(&sb, "This is an excerpt of the section %q. Call get_page with path=%s to get the whole section.\n", first.Heading, p.anchorURL(first.Anchor))
		} else {
			fmt.Fprintf(&sb, "This is an excerpt of the page. Call get_page with path=%s to get the whole page.\n", p.permalink)
		}
	}
	return sb.String(), nil
}

func outline(p *indexedPage, units []unit, unitParts []int) string {
	minLevel := 6
	for _, u := range units {
		if u.chunk >= 0 && p.chunks[u.chunk].Level > 0 {
			minLevel = min(minLevel, p.chunks[u.chunk].Level)
		}
	}
	var sb strings.Builder
	for i, u := range units {
		if u.chunk == -1 {
			fmt.Fprintf(&sb, "- %s (part %d)\n", childrenHeading(p), unitParts[i])
			continue
		}
		c := p.chunks[u.chunk]
		if c.Level == 0 {
			continue
		}
		sb.WriteString(strings.Repeat("  ", c.Level-minLevel))
		sb.WriteString("- ")
		sb.WriteString(c.Heading)
		if c.Anchor != "" {
			fmt.Fprintf(&sb, " {#%s}", c.Anchor)
		}
		fmt.Fprintf(&sb, " (part %d)\n", unitParts[i])
	}
	return sb.String()
}

func childrenHeading(p *indexedPage) string {
	if p.kind == kinds.KindHome {
		return "Sections and pages"
	}
	return "Pages in this section"
}

// childrenMarkdown returns a Markdown list of the pages and sections directly below p.
func (ix *index) childrenMarkdown(p *indexedPage) string {
	var sb strings.Builder
	sb.WriteString("## ")
	sb.WriteString(childrenHeading(p))
	sb.WriteString("\n\n")
	for i, ci := range p.children {
		if i == maxListEntries {
			fmt.Fprintf(&sb, "\n%d more pages are not listed. Use search_docs to find them.\n", len(p.children)-i)
			break
		}
		c := ix.pages[ci]
		fmt.Fprintf(&sb, "- [%s](%s)", linkTextEscaper.Replace(c.displayTitle()), c.permalink)
		if c.kind == kinds.KindSection {
			fmt.Fprintf(&sb, " (section, %s)", numPages(c.numPages))
		}
		if c.description != "" {
			sb.WriteString(": ")
			sb.WriteString(truncate(c.description, maxDescriptionLen))
		}
		sb.WriteByte('\n')
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func numPages(n int) string {
	if n == 1 {
		return "1 page"
	}
	return strconv.Itoa(n) + " pages"
}

var linkTextEscaper = strings.NewReplacer("[", `\[`, "]", `\]`)

// headerParams returns the page's params to include in the get_page header, nil if they are too large.
func (p *indexedPage) headerParams() hmaps.Params {
	if len(p.params) == 0 {
		return nil
	}
	if b, err := metadecoders.MarshalYAML(p.params); err != nil || len(b) > maxParamsSize {
		return nil
	}
	return p.params
}

func (p *indexedPage) headingsHint() string {
	var headings []string
	add := func(anchor, heading string) {
		headings = append(headings, cmp.Or(anchor, heading))
	}
	for _, c := range p.chunks {
		if c.Level > 0 {
			add(c.Anchor, c.Heading)
		}
		for _, ps := range c.Passages {
			if isPassageHeading(ps.Level) {
				add(ps.ID, ps.Heading)
			}
		}
	}
	if len(headings) > 50 {
		headings = append(headings[:50], "…")
	}
	if len(headings) == 0 {
		return "The page has no headings."
	}
	return "Available headings: " + strings.Join(headings, ", ")
}

func (p *indexedPage) displayTitle() string {
	if p.title != "" {
		return p.title
	}
	if p.kind == kinds.KindHome {
		return "Home"
	}
	return path.Base(p.path)
}

// paginate packs units into parts of at most maxSize bytes, splitting units that are too large.
// It returns the parts and the 1-based number of the part each unit starts in.
func paginate(units []unit, maxSize int) ([]string, []int) {
	pk := &packer{maxSize: maxSize}
	unitParts := make([]int, len(units))
	for i := 0; i < len(units); {
		// Keep a section without content, e.g. a heading followed by subsections, with the next section.
		md, j := units[i].markdown, i+1
		for j < len(units) && isHeadingLine(md[strings.LastIndexByte(md, '\n')+1:]) {
			md += "\n\n" + units[j].markdown
			j++
		}
		part := pk.add(md)
		for ; i < j; i++ {
			unitParts[i] = part
		}
	}
	return pk.done(), unitParts
}

// packer joins Markdown blocks with blank lines into parts of at most maxSize bytes.
type packer struct {
	maxSize int
	parts   []string
	sb      strings.Builder
}

// add adds md to the current part, or to a new part if it does not fit, and returns the part md starts in.
// Markdown larger than a part is split at blank lines outside of fenced code blocks,
// blocks that are still too large at line breaks. Fenced code blocks are then closed and reopened in the next part.
// Headings are kept with the block that follows them.
func (pk *packer) add(md string) int {
	if len(md) <= pk.maxSize {
		return pk.addBlock(md)
	}
	var start int
	var heading string
	for _, b := range markdownBlocks(md) {
		if b.fence == "" && isHeadingLine(b.text) {
			heading = joinBlocks(heading, b.text)
			continue
		}
		part := 0
		if text := joinBlocks(heading, b.text); len(text) <= pk.maxSize {
			part = pk.addBlock(text)
		} else {
			prefix := 0
			if heading != "" {
				prefix = len(heading) + 2
			}
			if pk.room()-prefix < max(pk.maxSize/8, firstPieceSize(b)) {
				// Too little room left to start the block in the current part.
				pk.flush()
			}
			for k, s := range splitBlock(b, pk.room()-prefix, pk.maxSize) {
				if k == 0 {
					part = pk.addBlock(joinBlocks(heading, s))
				} else {
					pk.addBlock(s)
				}
			}
		}
		start = cmp.Or(start, part)
		heading = ""
	}
	if heading != "" {
		start = cmp.Or(start, pk.addBlock(heading))
	}
	return start
}

// addBlock adds s to the current part, or to a new part if it does not fit, and returns the part.
func (pk *packer) addBlock(s string) int {
	if pk.room() < len(s) {
		pk.flush()
	}
	if pk.sb.Len() > 0 {
		pk.sb.WriteString("\n\n")
	}
	pk.sb.WriteString(s)
	return len(pk.parts) + 1
}

// room returns the size of the largest block that fits in the current part.
func (pk *packer) room() int {
	if pk.sb.Len() == 0 {
		return pk.maxSize
	}
	return pk.maxSize - pk.sb.Len() - 2
}

func (pk *packer) flush() {
	if pk.sb.Len() > 0 {
		pk.parts = append(pk.parts, pk.sb.String())
		pk.sb.Reset()
	}
}

func (pk *packer) done() []string {
	pk.flush()
	return pk.parts
}

func joinBlocks(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n\n" + b
}

// isHeadingLine reports whether s is an ATX heading, e.g. "## Heading {#id}".
func isHeadingLine(s string) bool {
	n := len(s) - len(strings.TrimLeft(s, "#"))
	return n >= 1 && n <= 6 && (len(s) == n || s[n] == ' ')
}

// mdBlock is a Markdown block separated from other blocks by blank lines.
type mdBlock struct {
	text  string
	fence string // The fence, e.g. ```, if this is a fenced code block.
}

func markdownBlocks(md string) []mdBlock {
	var (
		blocks []mdBlock
		lines  []string
		fence  string
	)
	flush := func() {
		if len(lines) > 0 {
			blocks = append(blocks, mdBlock{text: strings.Join(lines, "\n"), fence: fence})
			lines = nil
		}
	}
	for line := range strings.Lines(md) {
		line = strings.TrimRight(line, "\r\n")
		if fence != "" {
			lines = append(lines, line)
			if isFenceClose(line, fence) {
				flush()
				fence = ""
			}
			continue
		}
		if f := fenceOpen(line); f != "" {
			flush()
			fence = f
			lines = append(lines, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		lines = append(lines, line)
	}
	flush()
	return blocks
}

// fenceOpen returns the fence if line starts a fenced code block.
func fenceOpen(line string) string {
	s := strings.TrimLeft(line, " \t")
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return ""
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 || (s[0] == '`' && strings.ContainsRune(s[n:], '`')) {
		return ""
	}
	return s[:n]
}

func isFenceClose(line, fence string) bool {
	s := strings.TrimSpace(line)
	return len(s) >= len(fence) && strings.Trim(s, fence[:1]) == ""
}

// firstPieceSize returns the size of the smallest first piece splitBlock can split b into without breaking a line.
func firstPieceSize(b mdBlock) int {
	lines := strings.SplitN(b.text, "\n", 3)
	n := len(lines[0])
	if b.fence != "" && len(lines) > 1 {
		// The opening fence, the first line and the closing fence.
		n += len(lines[1]) + len(b.fence) + 2
	}
	return n
}

// splitBlock splits b at line breaks into pieces of at most first bytes for the first piece
// and maxSize bytes for the others. Lines that are too long are split at rune boundaries.
func splitBlock(b mdBlock, first, maxSize int) []string {
	lines := strings.Split(b.text, "\n")
	var open, close string
	if b.fence != "" {
		open, lines = lines[0], lines[1:]
		if len(lines) > 0 && isFenceClose(lines[len(lines)-1], b.fence) {
			close, lines = lines[len(lines)-1], lines[:len(lines)-1]
		} else {
			close = open[:len(open)-len(strings.TrimLeft(open, " \t"))] + b.fence
		}
	}
	overhead := 0
	if open != "" {
		overhead = len(open) + len(close) + 2
	}
	limit := max(first-overhead, 1)

	var (
		pieces []string
		cur    []string
		size   int
	)
	flush := func() {
		s := strings.Join(cur, "\n")
		if open != "" {
			s = open + "\n" + s + "\n" + close
		}
		pieces = append(pieces, s)
		cur, size = cur[:0], 0
		limit = max(maxSize-overhead, 1)
	}
	for _, line := range lines {
		if len(cur) > 0 && size+1+len(line) > limit {
			flush()
		}
		for len(line) > limit {
			i := limit
			for i > 0 && !utf8.RuneStart(line[i]) {
				i--
			}
			if i == 0 {
				_, i = utf8.DecodeRuneInString(line)
			}
			cur = append(cur, line[:i])
			flush()
			line = line[i:]
		}
		if len(cur) > 0 {
			size++
		}
		cur = append(cur, line)
		size += len(line)
	}
	if len(cur) > 0 {
		flush()
	}
	return pieces
}

// truncate collapses whitespace in s and truncates it to at most maxLen bytes, preferably at a space.
func truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	i := strings.LastIndexByte(s[:maxLen], ' ')
	if i < maxLen/2 {
		i = maxLen
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
	}
	return s[:i] + "…"
}

type tocOutput struct {
	Title       string       `json:"title"`
	URL         string       `json:"url"`
	Kind        string       `json:"kind" jsonschema:"The page kind: home, section or page."`
	Description string       `json:"description,omitempty"`
	Version     string       `json:"version,omitempty"`
	Language    string       `json:"language,omitempty"`
	Sites       []tocSite    `json:"sites,omitempty" jsonschema:"The versions and languages of the site, each with the URL of its home page."`
	Entries     []tocEntry   `json:"entries,omitempty" jsonschema:"The sections and pages directly below this page."`
	Headings    []tocHeading `json:"headings,omitempty" jsonschema:"The headings in this page."`
	Note        string       `json:"note,omitempty"`
	Warning     string       `json:"warning,omitempty" jsonschema:"Set if the content may be outdated because the last build failed."`
}

type tocSite struct {
	Title    string `json:"title"`
	URL      string `json:"url,omitempty"`
	Version  string `json:"version,omitempty"`
	Language string `json:"language,omitempty"`
	Default  bool   `json:"default,omitempty"`
}

type tocEntry struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Pages       int    `json:"pages,omitempty" jsonschema:"The number of pages in a section, including nested sections."`
}

type tocHeading struct {
	Level   int    `json:"level"`
	Heading string `json:"heading"`
	URL     string `json:"url"`
}

// toc returns the table of contents for page pi.
// For home pages, the versions and languages of the site are included.
func (ix *index) toc(pi int) tocOutput {
	p := ix.pages[pi]
	site := ix.sites[p.site]
	out := tocOutput{
		Title:       p.displayTitle(),
		URL:         p.permalink,
		Kind:        p.kind,
		Description: p.description,
	}
	if len(ix.versions) > 1 {
		out.Version = site.version
	}
	if len(ix.languages) > 1 {
		out.Language = site.language
	}
	if p.kind == kinds.KindHome && len(ix.sites) > 1 {
		for si, s := range ix.sites {
			ts := tocSite{Title: s.title, Default: si == ix.defaultSite}
			if s.home != -1 {
				ts.URL = ix.pages[s.home].permalink
			}
			if len(ix.versions) > 1 {
				ts.Version = s.version
			}
			if len(ix.languages) > 1 {
				ts.Language = s.language
			}
			out.Sites = append(out.Sites, ts)
		}
	}
	for i, ci := range p.children {
		if i == maxListEntries {
			out.Note = fmt.Sprintf("%d more entries are not listed. Use search_docs to find them.", len(p.children)-i)
			break
		}
		c := ix.pages[ci]
		out.Entries = append(out.Entries, tocEntry{
			Title:       c.displayTitle(),
			URL:         c.permalink,
			Kind:        c.kind,
			Description: truncate(c.description, maxDescriptionLen),
			Pages:       c.numPages,
		})
	}
	addHeading := func(level int, heading, anchor string) {
		if len(out.Headings) < maxListEntries {
			out.Headings = append(out.Headings, tocHeading{Level: level, Heading: heading, URL: p.anchorURL(anchor)})
		}
	}
	for _, c := range p.chunks {
		if c.Level > 0 {
			addHeading(c.Level, c.Heading, c.Anchor)
		}
		for _, ps := range c.Passages {
			if isPassageHeading(ps.Level) {
				addHeading(ps.Level, ps.Heading, ps.Anchor)
			}
		}
	}
	return out
}

type searchResult struct {
	Title   string `json:"title"`
	Heading string `json:"heading,omitempty" jsonschema:"The headings leading to the matching section of the page."`
	URL     string `json:"url" jsonschema:"The URL of the matching page or section. Pass it to get_page to read it."`
	Section string `json:"section,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

func (ix *index) searchResults(site *indexedSite, hits []searchHit, query string) []searchResult {
	terms, _ := queryTerms(query, site.english)
	results := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		p := ix.pages[h.page]
		c := &p.chunks[h.chunk]
		ps := c.Passages[h.passage]
		text := c.passageText(h.passage)
		if text == "" {
			// A heading followed by subheadings or definition lists.
			text = c.Text
		}
		if h.chunk == 0 && h.passage == 0 && p.description != "" && !strings.HasPrefix(text, p.description) {
			text = strings.TrimSpace(p.description + " " + text)
		}
		results = append(results, searchResult{
			Title:   p.displayTitle(),
			Heading: strings.Join(ps.Path, " > "),
			URL:     p.anchorURL(ps.Anchor),
			Section: p.section,
			Snippet: snippet(text, terms, site.english, snippetLen),
		})
	}
	return results
}

// notFoundError returns an error for a path that does not match any page with suggestions of pages to try.
func (ix *index) notFoundError(s string) error {
	msg := fmt.Sprintf("no page found for %q.", s)
	if urls := ix.suggest(s, 3); len(urls) > 0 {
		msg += " Did you mean: " + strings.Join(urls, ", ") + "?"
	}
	return errors.New(msg + " Use search_docs to find pages, or get_toc to browse.")
}

// suggest returns the URLs of up to n pages with a path similar to s,
// filled up with the results of a search for the words in s.
func (ix *index) suggest(s string, n int) []string {
	q, _, _ := strings.Cut(s, "#")
	if u, err := url.Parse(q); err == nil {
		q = u.Path
	}
	q = strings.Trim(normPath(q), "/")
	for _, suffix := range []string{"/_index.md", "/index.md", ".md", "/index.html", ".html"} {
		q = strings.TrimSuffix(q, suffix)
	}

	type candidate struct{ page, dist int }
	var candidates []candidate
	maxDist := max(2, utf8.RuneCountInString(q)/5)
	for pi, p := range ix.pages {
		dist := maxDist + 1
		for _, v := range []string{p.relPermalink, p.path} {
			dist = min(dist, editDistance(q, strings.Trim(normPath(v), "/"), maxDist))
		}
		if dist <= maxDist {
			candidates = append(candidates, candidate{pi, dist})
		}
	}
	otherSite := func(c candidate) int {
		if ix.pages[c.page].site == ix.defaultSite {
			return 0
		}
		return 1
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.dist, b.dist), cmp.Compare(otherSite(a), otherSite(b)), cmp.Compare(a.page, b.page))
	})

	var urls []string
	add := func(pi int) {
		if u := ix.pages[pi].permalink; len(urls) < n && !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	for _, c := range candidates {
		add(c.page)
	}
	if _, hits, err := ix.search(searchOptions{query: strings.NewReplacer("/", " ", "-", " ").Replace(q), limit: 10}); err == nil {
		for _, h := range hits {
			add(h.page)
		}
	}
	return urls
}

// editDistance returns the Levenshtein distance between a and b, or limit+1 if it's larger than limit.
func editDistance(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	if abs(len(ra)-len(rb)) > limit {
		return limit + 1
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		rowMin := i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return min(prev[len(rb)], limit+1)
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
