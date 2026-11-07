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
	"bytes"
	"cmp"
	"slices"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/marker"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// chunk is a part of a page's content. Content is split at top level h1, h2 and h3 headings.
type chunk struct {
	Level    int       // Heading level, 0 for the content before the first heading.
	Anchor   string    // The heading's id attribute.
	Heading  string    // The heading text.
	Path     []string  // Heading texts from the top level down to and including this chunk's heading.
	Markdown string    // The chunk as Markdown, including the heading.
	Text     string    // The chunk as plain text, excluding the heading.
	Anchors  []string  // The ids of the elements below the heading, e.g. of h4 headings.
	Passages []passage // The chunk's content split at h4, h5 and h6 headings and definition list entries.
}

// emptyChunks returns the chunks of a page without content.
func emptyChunks() []chunk {
	return []chunk{{Passages: make([]passage, 1)}}
}

// levelTerm is the passage level of definition list entries.
const levelTerm = 7

// passage is a part of a chunk that is searched and can be retrieved on its own,
// e.g. the content below a h4 heading or a definition list entry with its description.
// The first passage of a chunk is the content before any such heading or entry, which may be empty.
type passage struct {
	Level   int      // 4, 5 or 6 for headings, levelTerm for definition list entries, 0 for other content.
	ID      string   // The id of the heading or definition term, if any.
	Heading string   // The text of the heading or definition term(s).
	Anchor  string   // The anchor to link to, ID or the anchor of the closest heading above.
	Path    []string // The chunk's path followed by the headings and term leading to this passage.

	mdStart, mdEnd     int // The passage's range in the chunk's Markdown.
	textStart, textEnd int // The passage's range in the chunk's Text.
}

// closestHeading returns the text of the passage's heading or term, or of the closest heading above it.
func (p *passage) closestHeading() string {
	if len(p.Path) == 0 {
		return ""
	}
	return p.Path[len(p.Path)-1]
}

func isPassageHeading(level int) bool {
	return level >= 4 && level <= 6
}

func (c *chunk) passageText(i int) string {
	return c.Text[c.Passages[i].textStart:c.Passages[i].textEnd]
}

// passageRange returns the range of passages [i, end) that belong to passage i,
// e.g. a h4 heading and the h5 headings and definition list entries below it.
func (c *chunk) passageRange(i int) (end int) {
	end = i + 1
	if level := c.Passages[i].Level; isPassageHeading(level) {
		for end < len(c.Passages) && !(isPassageHeading(c.Passages[end].Level) && c.Passages[end].Level <= level) {
			end++
		}
	}
	return end
}

func newMarkdownConverter() *htmltomarkdown.Converter {
	conv := htmltomarkdown.NewConverter(
		htmltomarkdown.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			table.NewTablePlugin(),
			strikethrough.NewStrikethroughPlugin(),
		),
	)
	conv.Register.RendererFor("dt", htmltomarkdown.TagTypeBlock, renderDefinitionTerm, htmltomarkdown.PriorityStandard)
	conv.Register.RendererFor("dd", htmltomarkdown.TagTypeBlock, renderDefinitionDescription, htmltomarkdown.PriorityStandard)
	return conv
}

// renderDefinitionTerm renders a definition term on its own line, as in the Markdown definition list syntax:
//
//	Term
//	: Description
func renderDefinitionTerm(ctx htmltomarkdown.Context, w htmltomarkdown.Writer, n *html.Node) htmltomarkdown.RenderStatus {
	var buf bytes.Buffer
	ctx.RenderChildNodes(ctx, &buf, n)
	term := strings.Join(strings.Fields(buf.String()), " ")
	if term == "" {
		return htmltomarkdown.RenderSuccess
	}
	if prev := previousElement(n); prev == nil || prev.DataAtom != atom.Dt {
		w.WriteString("\n\n")
	}
	w.WriteString(term)
	w.WriteString("\n")
	return htmltomarkdown.RenderSuccess
}

// renderDefinitionDescription renders a definition description prefixed with ": ",
// with the following lines indented, as in the Markdown definition list syntax.
func renderDefinitionDescription(ctx htmltomarkdown.Context, w htmltomarkdown.Writer, n *html.Node) htmltomarkdown.RenderStatus {
	var buf bytes.Buffer
	ctx.RenderChildNodes(ctx, &buf, n)
	desc := bytes.TrimSpace(buf.Bytes())
	if len(desc) == 0 {
		return htmltomarkdown.RenderSuccess
	}
	const indent = "  "
	desc = bytes.ReplaceAll(desc, marker.BytesMarkerCodeBlockNewline, append(slices.Clone(marker.BytesMarkerCodeBlockNewline), indent...))
	w.WriteString(": ")
	for i, line := range bytes.Split(desc, []byte("\n")) {
		if i > 0 {
			w.WriteString("\n")
			if len(bytes.TrimSpace(line)) > 0 {
				w.WriteString(indent)
			}
		}
		w.Write(line)
	}
	w.WriteString("\n")
	return htmltomarkdown.RenderSuccess
}

func previousElement(n *html.Node) *html.Node {
	for c := n.PrevSibling; c != nil; c = c.PrevSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

// splitContent splits the rendered HTML content of a page into chunks and converts them to Markdown.
// Relative links are resolved against permalink.
// The first chunk is always the content before the first heading, which may be empty.
func splitContent(conv *htmltomarkdown.Converter, content, permalink string) ([]chunk, error) {
	nodes, err := html.ParseFragment(strings.NewReader(content), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		return nil, err
	}

	var (
		chunks []chunk
		cur    chunk
		body   []*html.Node
		path   []chunk
	)

	flush := func() error {
		var md, text strings.Builder
		if cur.Level > 0 {
			md.WriteString(strings.Repeat("#", cur.Level) + " " + cur.Heading)
			if cur.Anchor != "" {
				md.WriteString(" {#" + cur.Anchor + "}")
			}
		}
		for _, pb := range splitPassages(body) {
			p := pb.passage
			if len(pb.nodes) > 0 {
				doc := &html.Node{Type: html.DocumentNode}
				b := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
				doc.AppendChild(b)
				for _, n := range pb.nodes {
					b.AppendChild(n)
				}
				p.textStart, p.textEnd = appendPart(&text, " ", textContent(b))
				addHeadingAnchors(b)
				mdb, err := conv.ConvertNode(doc, htmltomarkdown.WithDomain(permalink))
				if err != nil {
					return err
				}
				p.mdStart, p.mdEnd = appendPart(&md, "\n\n", strings.TrimSpace(string(mdb)))
			} else {
				p.textStart, p.textEnd = text.Len(), text.Len()
				p.mdStart, p.mdEnd = md.Len(), md.Len()
			}
			cur.Passages = append(cur.Passages, p)
		}
		cur.Markdown, cur.Text = md.String(), text.String()
		cur.setPassagePaths()
		chunks = append(chunks, cur)
		body = nil
		return nil
	}

	for _, n := range nodes {
		if isNoise(n) {
			continue
		}
		if level := headingLevel(n); level > 0 && level <= 3 {
			if err := flush(); err != nil {
				return nil, err
			}
			cur = chunk{Level: level, Anchor: getAttr(n, "id")}
			removeNoise(n)
			removeSelfLinks(n, cur.Anchor)
			cur.Heading = strings.Trim(textContent(n), " #¶§")
			for len(path) > 0 && path[len(path)-1].Level >= level {
				path = path[:len(path)-1]
			}
			path = append(path, cur)
			cur.Path = make([]string, len(path))
			for i, c := range path {
				cur.Path[i] = c.Heading
			}
			continue
		}
		removeNoise(n)
		cur.Anchors = appendIDs(cur.Anchors, n)
		body = append(body, n)
	}
	if err := flush(); err != nil {
		return nil, err
	}

	return chunks, nil
}

type passageBlock struct {
	passage passage
	nodes   []*html.Node
}

// splitPassages splits the top level nodes of a chunk into passages at h4, h5 and h6 headings
// and definition list entries. The first passage is the content before them, which may be empty.
func splitPassages(nodes []*html.Node) []passageBlock {
	blocks := []passageBlock{{}}
	start := func(p passage) {
		blocks = append(blocks, passageBlock{passage: p})
	}
	add := func(n *html.Node) {
		blocks[len(blocks)-1].nodes = append(blocks[len(blocks)-1].nodes, n)
	}
	for _, n := range nodes {
		switch {
		case isPassageHeading(headingLevel(n)):
			id := getAttr(n, "id")
			removeSelfLinks(n, id)
			start(passage{Level: headingLevel(n), ID: id, Heading: strings.Trim(textContent(n), " #¶§")})
			add(n)
		case n.Type == html.ElementNode && n.DataAtom == atom.Dl:
			for _, entry := range splitDefinitionList(n) {
				if p, ok := termPassage(entry); ok {
					start(p)
				} else if blocks[len(blocks)-1].passage.Level == levelTerm {
					start(passage{})
				}
				add(entry)
			}
		default:
			if blocks[len(blocks)-1].passage.Level == levelTerm && (n.Type != html.TextNode || strings.TrimSpace(n.Data) != "") {
				// The content after a definition list.
				start(passage{})
			}
			add(n)
		}
	}
	return blocks
}

// termPassage returns the passage for the definition list entry with the given terms and descriptions.
// Entries without terms and entries with terms that link elsewhere, as in an index, are not passages of their own.
func termPassage(entry *html.Node) (passage, bool) {
	p := passage{Level: levelTerm}
	var terms []string
	for c := entry.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.DataAtom != atom.Dt {
			continue
		}
		if isLink(c) {
			return p, false
		}
		if p.ID == "" {
			p.ID = getAttr(c, "id")
		}
		terms = append(terms, textContent(c))
	}
	p.Heading = strings.Join(terms, ", ")
	return p, len(terms) > 0
}

// isLink reports whether the content of n is a link to another page or site.
func isLink(n *html.Node) bool {
	var a *html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode && strings.TrimSpace(c.Data) == "":
		case c.Type == html.ElementNode && c.DataAtom == atom.A && a == nil:
			a = c
		default:
			return false
		}
	}
	if a == nil {
		return false
	}
	href := getAttr(a, "href")
	return href != "" && !strings.HasPrefix(href, "#")
}

// splitDefinitionList splits dl into one definition list per entry, i.e. per group of terms and their descriptions.
func splitDefinitionList(dl *html.Node) []*html.Node {
	var (
		entries []*html.Node
		entry   *html.Node
		prevDT  bool
	)
	for c := dl.FirstChild; c != nil; {
		next := c.NextSibling
		dl.RemoveChild(c)
		if c.Type == html.ElementNode {
			isDT := c.DataAtom == atom.Dt
			if entry == nil || (isDT && !prevDT) {
				entry = &html.Node{Type: html.ElementNode, Data: dl.Data, DataAtom: dl.DataAtom, Attr: dl.Attr}
				entries = append(entries, entry)
			}
			prevDT = isDT
		}
		if entry != nil {
			entry.AppendChild(c)
		}
		c = next
	}
	return entries
}

// setPassagePaths sets the path and anchor of the passages in c.
func (c *chunk) setPassagePaths() {
	type parent struct {
		level  int
		anchor string
		path   []string
	}
	stack := []parent{{level: c.Level, anchor: c.Anchor, path: c.Path}}
	for i := range c.Passages {
		p := &c.Passages[i]
		if isPassageHeading(p.Level) {
			for len(stack) > 1 && stack[len(stack)-1].level >= p.Level {
				stack = stack[:len(stack)-1]
			}
		}
		top := stack[len(stack)-1]
		p.Anchor = cmp.Or(p.ID, top.anchor)
		p.Path = top.path
		if p.Level > 0 {
			p.Path = slices.Concat(top.path, []string{p.Heading})
		}
		if isPassageHeading(p.Level) {
			stack = append(stack, parent{level: p.Level, anchor: p.Anchor, path: p.Path})
		}
	}
}

// appendPart appends s to sb, separated by sep from any previous content, and returns its range in sb.
func appendPart(sb *strings.Builder, sep, s string) (start, end int) {
	if s != "" && sb.Len() > 0 {
		sb.WriteString(sep)
	}
	start = sb.Len()
	sb.WriteString(s)
	return start, sb.Len()
}

func headingLevel(n *html.Node) int {
	if n.Type != html.ElementNode {
		return 0
	}
	switch n.DataAtom {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	case atom.H6:
		return 6
	}
	return 0
}

// addHeadingAnchors adds the ids of h4, h5 and h6 headings below n to their text, e.g. "Heading {#id}".
func addHeadingAnchors(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.H4, atom.H5, atom.H6:
			id := getAttr(c, "id")
			removeSelfLinks(c, id)
			if id != "" {
				c.AppendChild(&html.Node{Type: html.TextNode, Data: " {#" + id + "}"})
			}
		default:
			addHeadingAnchors(c)
		}
	}
}

// removeSelfLinks removes links to the heading n itself, e.g. added by heading render hooks.
// The link text is kept unless it's only a symbol, e.g. # or ¶.
func removeSelfLinks(n *html.Node, anchor string) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && c.DataAtom == atom.A {
			if href := getAttr(c, "href"); href == "#" || (anchor != "" && href == "#"+anchor) {
				if strings.ContainsFunc(textContent(c), isWordRune) {
					for gc := c.FirstChild; gc != nil; gc = c.FirstChild {
						c.RemoveChild(gc)
						n.InsertBefore(gc, c)
					}
				}
				n.RemoveChild(c)
				c = next
				continue
			}
		}
		removeSelfLinks(c, anchor)
		c = next
	}
}

// isNoise reports whether n is an element that does not carry any content useful as text,
// e.g. scripts, forms, buttons and icons.
func isNoise(n *html.Node) bool {
	switch n.Type {
	case html.CommentNode, html.DoctypeNode:
		return true
	case html.ElementNode:
	default:
		return false
	}
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Button, atom.Form,
		atom.Input, atom.Select, atom.Textarea, atom.Iframe, atom.Canvas, atom.Object, atom.Embed:
		return true
	}
	if getAttr(n, "aria-hidden") == "true" {
		return true
	}
	_, hidden := lookupAttr(n, "hidden")
	return hidden
}

func removeNoise(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if isNoise(c) {
			n.RemoveChild(c)
		} else {
			removeNoise(c)
		}
		c = next
	}
}

// textContent returns the text of n with whitespace collapsed.
func textContent(n *html.Node) string {
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			sb.WriteString(n.Data)
			return
		case html.ElementNode:
			if !isInline(n) {
				sb.WriteByte(' ')
				defer sb.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

func appendIDs(ids []string, n *html.Node) []string {
	if n.Type == html.ElementNode {
		if id := getAttr(n, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		ids = appendIDs(ids, c)
	}
	return ids
}

func isInline(n *html.Node) bool {
	switch n.DataAtom {
	case atom.A, atom.Abbr, atom.B, atom.Bdi, atom.Bdo, atom.Cite, atom.Code, atom.Data, atom.Del, atom.Dfn,
		atom.Em, atom.I, atom.Ins, atom.Kbd, atom.Mark, atom.Q, atom.S, atom.Samp, atom.Small, atom.Span,
		atom.Strong, atom.Sub, atom.Sup, atom.Time, atom.U, atom.Var:
		return true
	}
	return false
}

func getAttr(n *html.Node, key string) string {
	v, _ := lookupAttr(n, key)
	return v
}

func lookupAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}
