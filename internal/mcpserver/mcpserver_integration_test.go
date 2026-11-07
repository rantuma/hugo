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

package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/gohugoio/hugo/common/loggers"
	"github.com/gohugoio/hugo/hugolib"
	"github.com/gohugoio/hugo/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type testClient struct {
	b  *hugolib.IntegrationTestBuilder
	s  *mcpserver.Server
	cs *mcp.ClientSession
}

// newTestClient indexes the site built in b and connects an MCP client to a server for it.
func newTestClient(t testing.TB, b *hugolib.IntegrationTestBuilder) *testClient {
	c := connect(t, b)
	c.update()
	return c
}

// connect connects an MCP client to a new server for the site built in b without indexing it.
func connect(t testing.TB, b *hugolib.IntegrationTestBuilder) *testClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := mcpserver.New(b.H.Configs, loggers.NewDefault())
	st, ct := mcp.NewInMemoryTransports()
	go s.Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1.0.0"}, nil).Connect(ctx, ct, nil)
	b.Assert(err, qt.IsNil)
	t.Cleanup(func() {
		cs.Close()
		cancel()
	})
	return &testClient{b: b, s: s, cs: cs}
}

func (c *testClient) update() {
	c.b.Helper()
	c.b.Assert(c.s.Update(context.Background(), c.b.H), qt.IsNil)
}

// call calls the tool and returns its text output and whether it's an error.
func (c *testClient) call(tool string, args map[string]any) (string, bool) {
	c.b.Helper()
	res, err := c.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	c.b.Assert(err, qt.IsNil)
	var sb strings.Builder
	for _, content := range res.Content {
		sb.WriteString(content.(*mcp.TextContent).Text)
	}
	return sb.String(), res.IsError
}

func (c *testClient) callOK(tool string, args map[string]any) string {
	c.b.Helper()
	s, isErr := c.call(tool, args)
	c.b.Assert(isErr, qt.IsFalse, qt.Commentf("%s", s))
	return s
}

func (c *testClient) callErr(tool string, args map[string]any) string {
	c.b.Helper()
	s, isErr := c.call(tool, args)
	c.b.Assert(isErr, qt.IsTrue, qt.Commentf("%s", s))
	return s
}

func (c *testClient) search(args map[string]any) searchOutput {
	c.b.Helper()
	var out searchOutput
	c.b.Assert(json.Unmarshal([]byte(c.callOK("search_docs", args)), &out), qt.IsNil)
	return out
}

func (c *testClient) assertJSON(got, want string) {
	c.b.Helper()
	var g, w any
	c.b.Assert(json.Unmarshal([]byte(got), &g), qt.IsNil)
	c.b.Assert(json.Unmarshal([]byte(want), &w), qt.IsNil)
	c.b.Assert(g, qt.DeepEquals, w, qt.Commentf("%s", got))
}

type searchResult struct {
	Title   string `json:"title"`
	Heading string `json:"heading"`
	URL     string `json:"url"`
	Section string `json:"section"`
	Snippet string `json:"snippet"`
}

type searchOutput struct {
	Version  string         `json:"version"`
	Language string         `json:"language"`
	Results  []searchResult `json:"results"`
	Note     string         `json:"note"`
	Warning  string         `json:"warning"`
}

func (o searchOutput) urls() []string {
	var urls []string
	for _, r := range o.Results {
		urls = append(urls, r.URL)
	}
	return urls
}

const basicSite = `
-- hugo.toml --
baseURL = "https://example.org/"
title = "My Docs"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
-- content/_index.md --
---
title: Home
---
Welcome to the docs.
-- content/functions/_index.md --
---
title: Functions
description: Template functions.
---
-- content/functions/collections/_index.md --
---
title: collections
---
-- content/functions/collections/Where.md --
---
title: collections.Where
description: Returns the given collection, removing elements that do not satisfy the comparison condition.
---
-- content/functions/strings/_index.md --
---
title: strings
---
-- content/functions/strings/Replace.md --
---
title: strings.Replace
description: Returns a copy of INPUT, replacing all occurrences of OLD with NEW.
keywords: [substitute]
aliases: [/functions/replace/]
params:
  functions_and_methods:
    signatures: ['strings.Replace INPUT OLD NEW [LIMIT]']
---
Use it like this:

## Examples

` + "```go-html-template" + `
{{ strings.Replace "Batman and Robin" "Robin" "Catwoman" }}
` + "```" + `

### Limit

Pass a LIMIT to limit the number of replacements.

#### Negative limit {#negative}

A negative limit means no limit.

## See also

Other [functions](/functions/).
-- content/configuration/markup.md --
---
title: Configure markup
description: Configure rendering of markup to HTML.
---
## Goldmark

### Renderer

Set ` + "`markup.goldmark.renderer.unsafe`" + ` to true to render raw HTML.

{{< note >}}Restart the server after changing this setting.{{< /note >}}

| Setting | Default |
|---------|---------|
| unsafe  | false   |
-- layouts/_markup/render-heading.html --
{{- if eq .Level 3 -}}
<h3 id="{{ .Anchor }}"><a href="#{{ .Anchor }}">{{ .Text }}</a></h3>
{{- else -}}
<h{{ .Level }} id="{{ .Anchor }}">{{ .Text }} <a class="anchor" href="#{{ .Anchor }}">#</a></h{{ .Level }}>
{{- end -}}
-- layouts/_shortcodes/note.html --
<div class="note"><svg aria-hidden="true"><path d="M0 0"/></svg><button>Copy</button><script>alert("noise")</script><p>{{ .Inner }}</p></div>
-- layouts/all.html --
{{ .Content }}
`

const replaceURL = "https://example.org/functions/strings/replace/"

// See issue 14747.
func TestSearchDocs(t *testing.T) {
	t.Parallel()
	b := hugolib.Test(t, basicSite, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	out := c.search(map[string]any{"query": "strings.Replace"})
	b.Assert(out.Results[0], qt.DeepEquals, searchResult{
		Title:   "strings.Replace",
		URL:     replaceURL,
		Section: "functions",
		Snippet: "Returns a copy of INPUT, replacing all occurrences of OLD with NEW. Use it like this:",
	})
	b.Assert(out.Results[1].URL, qt.Equals, replaceURL+"#examples")
	b.Assert(out.Results[1].Heading, qt.Equals, "Examples")

	for _, q := range []string{"markup.goldmark.renderer.unsafe", "renderer.unsafe"} {
		out = c.search(map[string]any{"query": q})
		b.Assert(out.Results[0].URL, qt.Equals, "https://example.org/configuration/markup/#renderer")
		b.Assert(out.Results[0].Heading, qt.Equals, "Goldmark > Renderer")
		b.Assert(out.Results[0].Snippet, qt.Equals, "Set markup.goldmark.renderer.unsafe to true to render raw HTML. "+
			"Restart the server after changing this setting. Setting Default unsafe false")
	}

	for _, q := range []string{"replace", "substitute", "How do I replace text?"} {
		b.Assert(c.search(map[string]any{"query": q}).Results[0].URL, qt.Equals, replaceURL, qt.Commentf(q))
	}
	// A query with only stop words.
	b.Assert(c.search(map[string]any{"query": "where"}).Results[0].URL, qt.Equals, "https://example.org/functions/collections/where/")

	out = c.search(map[string]any{"query": "limit", "section": "functions"})
	b.Assert(out.Results[0].URL, qt.Equals, replaceURL+"#limit")
	b.Assert(out.Results[0].Heading, qt.Equals, "Examples > Limit")
	b.Assert(c.search(map[string]any{"query": "returns", "section": "/functions/collections/"}).urls(), qt.DeepEquals,
		[]string{"https://example.org/functions/collections/where/"})
	b.Assert(c.search(map[string]any{"query": "replace", "limit": 1}).Results, qt.HasLen, 1)

	out = c.search(map[string]any{"query": "limit", "section": "configuration"})
	b.Assert(out.Results, qt.HasLen, 0)
	b.Assert(out.Note, qt.Equals, "No matches. Try other or fewer words, or browse with get_toc.")

	b.Assert(c.callErr("search_docs", map[string]any{"query": "limit", "section": "nope"}), qt.Equals,
		`unknown section "nope", top level sections: configuration, functions`)
	b.Assert(c.callErr("search_docs", map[string]any{"query": "!!!"}), qt.Equals, `the query "!!!" contains no searchable words`)
	b.Assert(c.callErr("search_docs", map[string]any{"query": strings.Repeat("Ab.", 334)}), qt.Equals,
		"the query is too long, the maximum length is 1000 bytes")
}

// Identical sections in different pages, e.g. from a shared include, are only returned once.
// See issue 14747.
func TestSearchDocsSkipsDuplicateSections(t *testing.T) {
	t.Parallel()
	files := `
-- hugo.toml --
baseURL = "https://example.org/"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
-- content/a.md --
---
title: Page A
---
## Note

The frobnicate setting is deprecated.
-- content/b.md --
---
title: Page B
---
## Note

The frobnicate setting is deprecated.
-- layouts/all.html --
{{ .Content }}
`
	b := hugolib.Test(t, files, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	b.Assert(c.search(map[string]any{"query": "frobnicate"}).urls(), qt.DeepEquals, []string{"https://example.org/a/#note"})
}

// A section whose page title and headings together match the query ranks above
// a short section that just mentions it.
// See issue 14747.
func TestSearchDocsRanksByBreadcrumb(t *testing.T) {
	t.Parallel()
	files := `
-- hugo.toml --
baseURL = "https://example.org/"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
-- content/front-matter.md --
---
title: Front matter
---
Front matter is metadata at the top of each content file.

## Dates

When populating a date field, whether a custom page parameter or one of the four
predefined fields, use one of these parsable formats:

- 2023-10-15T13:18:50-07:00
- 2023-10-15T13:18:50
- 2023-10-15
- 15 Oct 2023

To override the default time zone, set the timeZone in your project configuration.
-- content/where.md --
---
title: collections.Where
---
Returns a slice by filtering the given slice.

## Date comparison

Compare front matter dates, or custom front matter dates.
-- layouts/all.html --
{{ .Content }}
`
	b := hugolib.Test(t, files, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	b.Assert(c.search(map[string]any{"query": "front matter dates"}).urls(), qt.DeepEquals, []string{
		"https://example.org/front-matter/#dates",
		"https://example.org/where/#date-comparison",
		"https://example.org/front-matter/",
	})
}

// See issue 14747.
func TestGetPage(t *testing.T) {
	t.Parallel()
	b := hugolib.Test(t, basicSite, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	page := c.callOK("get_page", map[string]any{"path": replaceURL})
	b.Assert(page, qt.Equals, `---
title: strings.Replace
url: https://example.org/functions/strings/replace/
description: Returns a copy of INPUT, replacing all occurrences of OLD with NEW.
section: functions
params:
  functions_and_methods:
    signatures:
    - strings.Replace INPUT OLD NEW [LIMIT]
---

Use it like this:

## Examples {#examples}

`+"```go-html-template"+`
{{ strings.Replace "Batman and Robin" "Robin" "Catwoman" }}
`+"```"+`

### Limit {#limit}

Pass a LIMIT to limit the number of replacements.

#### Negative limit {#negative}

A negative limit means no limit.

## See also {#see-also}

Other [functions](https://example.org/functions/).
`)

	for _, path := range []string{
		"/functions/strings/replace",
		"/Functions/Strings/Replace/index.html",
		"/functions/replace/", // Alias.
		"content/functions/strings/Replace.md",
		"functions/strings/Replace.md",
		replaceURL + "?foo=bar#nope", // The whole page is returned for unknown anchors.
	} {
		b.Assert(c.callOK("get_page", map[string]any{"path": path}), qt.Equals, page, qt.Commentf(path))
	}

	limit := c.callOK("get_page", map[string]any{"path": replaceURL + "#limit"})
	b.Assert(limit, qt.Equals, `---
title: strings.Replace
url: 'https://example.org/functions/strings/replace/#limit'
heading: Examples > Limit
description: Returns a copy of INPUT, replacing all occurrences of OLD with NEW.
section: functions
params:
  functions_and_methods:
    signatures:
    - strings.Replace INPUT OLD NEW [LIMIT]
---

### Limit {#limit}

Pass a LIMIT to limit the number of replacements.

#### Negative limit {#negative}

A negative limit means no limit.
`)
	for _, args := range []map[string]any{
		{"path": replaceURL, "heading": "limit"},
		{"path": replaceURL, "heading": "#limit"},
		{"path": replaceURL, "heading": "Limit"},
	} {
		b.Assert(c.callOK("get_page", args), qt.Equals, limit, qt.Commentf("%v", args))
	}

	negative := c.callOK("get_page", map[string]any{"path": replaceURL + "#negative"})
	b.Assert(negative, qt.Equals, `---
title: strings.Replace
url: 'https://example.org/functions/strings/replace/#negative'
heading: Examples > Limit > Negative limit
description: Returns a copy of INPUT, replacing all occurrences of OLD with NEW.
section: functions
params:
  functions_and_methods:
    signatures:
    - strings.Replace INPUT OLD NEW [LIMIT]
---

#### Negative limit {#negative}

A negative limit means no limit.

---

This is an excerpt of the section "Limit". Call get_page with path=https://example.org/functions/strings/replace/#limit to get the whole section.
`)
	b.Assert(c.callOK("get_page", map[string]any{"path": replaceURL, "heading": "Negative limit"}), qt.Equals, negative)

	examples := c.callOK("get_page", map[string]any{"path": replaceURL, "heading": "Examples"})
	b.Assert(examples, qt.Contains, "\nheading: Examples\n")
	b.Assert(examples, qt.Contains, "### Limit {#limit}")
	b.Assert(examples, qt.Not(qt.Contains), "See also")

	b.Assert(c.callOK("get_page", map[string]any{"path": "/functions/"}), qt.Equals, `---
title: Functions
url: https://example.org/functions/
description: Template functions.
section: functions
---

## Pages in this section

- [collections](https://example.org/functions/collections/) (section, 1 page)
- [strings](https://example.org/functions/strings/) (section, 1 page)
`)

	markup := c.callOK("get_page", map[string]any{"path": "configuration/markup.md"})
	b.Assert(markup, qt.Contains, "## Goldmark {#goldmark}\n\n### Renderer {#renderer}\n\n"+
		"Set `markup.goldmark.renderer.unsafe` to true to render raw HTML.\n\n"+
		"Restart the server after changing this setting.\n\n"+
		"| Setting | Default |\n")
	b.Assert(markup, qt.Not(qt.Contains), "Copy")
	b.Assert(markup, qt.Not(qt.Contains), "noise")

	b.Assert(c.callErr("get_page", map[string]any{"path": "/functions/strings/replac/"}), qt.Equals,
		`no page found for "/functions/strings/replac/". Did you mean: https://example.org/functions/strings/replace/, `+
			`https://example.org/functions/, https://example.org/functions/strings/? Use search_docs to find pages, or get_toc to browse.`)
	b.Assert(c.callErr("get_page", map[string]any{"path": replaceURL, "heading": "nope"}), qt.Equals,
		`heading "nope" not found in https://example.org/functions/strings/replace/. Available headings: examples, limit, negative, see-also`)
	b.Assert(c.callErr("get_page", map[string]any{"path": replaceURL, "part": 2}), qt.Equals,
		"part 2 does not exist, the page has 1 part(s)")
	b.Assert(c.callErr("get_page", map[string]any{"path": strings.Repeat("/a", 501)}), qt.Equals,
		"the path is too long, the maximum length is 1000 bytes")
}

// Long pages are split into parts at section boundaries without breaking code blocks.
// See issue 14747.
func TestGetPageParts(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString("---\ntitle: Long\n---\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&sb, "## Section %d\n\n%s\n\n", i, strings.Repeat("Lorem ipsum dolor sit amet. ", 50))
	}
	sb.WriteString("## Code\n\n```go\n")
	for i := range 2000 {
		fmt.Fprintf(&sb, "fmt.Println(%d)\n", i)
	}
	sb.WriteString("```\n")
	files := `
-- hugo.toml --
baseURL = "https://example.org/"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
-- layouts/all.html --
{{ .Content }}
-- content/long.md --
` + sb.String()

	b := hugolib.Test(t, files, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	var all strings.Builder
	for part := 1; part <= 4; part++ {
		s := c.callOK("get_page", map[string]any{"path": "/long/", "part": part})
		b.Assert(s, qt.Contains, fmt.Sprintf("\npart: %d of 4\n", part))
		b.Assert(len(s) < 32000, qt.IsTrue, qt.Commentf("part %d has %d bytes", part, len(s)))
		b.Assert(strings.Count(s, "```")%2, qt.Equals, 0, qt.Commentf("unbalanced code fences in part %d", part))
		switch part {
		case 1:
			b.Assert(s, qt.Contains, "This is part 1 of 4. Call get_page again with part=2 to continue. "+
				"Outline with the part each section starts in:\n\n- Section 1 {#section-1} (part 1)\n")
			b.Assert(s, qt.Contains, "\n- Code {#code} (part 3)\n")
		case 3:
			// The heading is kept with the code block.
			b.Assert(s, qt.Contains, "---\n\n## Code {#code}\n\n```go\nfmt.Println(0)\n")
		case 4:
			b.Assert(s, qt.Contains, "This is the last part (4 of 4).")
			b.Assert(s, qt.Not(qt.Contains), "Outline")
		}
		all.WriteString(s)
	}
	for i := 1; i <= 40; i++ {
		b.Assert(strings.Count(all.String(), fmt.Sprintf("## Section %d {#section-%d}\n", i, i)), qt.Equals, 1)
	}
	for i := range 2000 {
		b.Assert(strings.Count(all.String(), fmt.Sprintf("\nfmt.Println(%d)\n", i)), qt.Equals, 1)
	}
	b.Assert(c.callErr("get_page", map[string]any{"path": "/long/", "part": 5}), qt.Equals, "part 5 does not exist, the page has 4 part(s)")

	code := c.callOK("get_page", map[string]any{"path": "/long/", "heading": "code"})
	b.Assert(code, qt.Contains, "\npart: 1 of 2\n")
	b.Assert(code, qt.Contains, "---\n\n## Code {#code}\n\n```go\nfmt.Println(0)\n")
}

// Definition list entries, e.g. settings, are searched and retrieved on their own.
// See issue 14747.
func TestDefinitionLists(t *testing.T) {
	t.Parallel()
	files := `
-- hugo.toml --
baseURL = "https://example.org/"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
[markup.goldmark.parser]
autoDefinitionTermID = true
-- content/settings.md --
---
title: All settings
---
## Settings

` + "`baseURL`" + `
: The absolute URL of the published site.

` + "`disableKinds`" + `
: A slice of page kinds to disable during the build.

  For example:

  ` + "```toml" + `
  disableKinds = ['rss']
  ` + "```" + `

` + "`timeout`" + `
: The maximum time to wait for a page to render.

Hugo has sensible defaults.
-- content/functions.md --
---
title: Functions index
---
[` + "`strings.Replace`" + `](/functions/strings/replace/)
: Returns a copy of INPUT, replacing all occurrences of OLD with NEW.
-- layouts/all.html --
{{ .Content }}
`
	b := hugolib.Test(t, files, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	// Entries with terms that link elsewhere, as in an index, are not passages of their own.
	b.Assert(c.search(map[string]any{"query": "occurrences"}).urls(), qt.DeepEquals, []string{"https://example.org/functions/"})

	const settingsURL = "https://example.org/settings/"
	out := c.search(map[string]any{"query": "disableKinds"})
	b.Assert(out.Results[0].URL, qt.Equals, settingsURL+"#disablekinds")
	b.Assert(out.Results[0].Heading, qt.Equals, "Settings > disableKinds")
	b.Assert(out.Results[0].Snippet, qt.Equals, "disableKinds A slice of page kinds to disable during the build. For example: disableKinds = ['rss']")
	b.Assert(c.search(map[string]any{"query": "sensible defaults"}).urls(), qt.DeepEquals, []string{settingsURL + "#settings"})

	entry := c.callOK("get_page", map[string]any{"path": settingsURL + "#disablekinds"})
	b.Assert(entry, qt.Equals, `---
title: All settings
url: 'https://example.org/settings/#disablekinds'
heading: Settings > disableKinds
---

`+"`disableKinds`"+`
: A slice of page kinds to disable during the build.

  For example:

  `+"```toml"+`
  disableKinds = ['rss']
  `+"```"+`

---

This is an excerpt of the section "Settings". Call get_page with path=https://example.org/settings/#settings to get the whole section.
`)
	b.Assert(c.callOK("get_page", map[string]any{"path": settingsURL, "heading": "disableKinds"}), qt.Equals, entry)

	b.Assert(c.callOK("get_page", map[string]any{"path": settingsURL}), qt.Contains, `## Settings {#settings}

`+"`baseURL`"+`
: The absolute URL of the published site.

`+"`disableKinds`"+`
: A slice of page kinds to disable during the build.

  For example:

  `+"```toml"+`
  disableKinds = ['rss']
  `+"```"+`

`+"`timeout`"+`
: The maximum time to wait for a page to render.

Hugo has sensible defaults.
`)
}

// See issue 14747.
func TestGetTOC(t *testing.T) {
	t.Parallel()
	b := hugolib.Test(t, basicSite, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	c.assertJSON(c.callOK("get_toc", nil), `{
"title": "Home", "url": "https://example.org/", "kind": "home",
"entries": [
  {"title": "Configurations", "url": "https://example.org/configuration/", "kind": "section", "pages": 1},
  {"title": "Functions", "url": "https://example.org/functions/", "kind": "section", "description": "Template functions.", "pages": 2}
]}`)
	c.assertJSON(c.callOK("get_toc", map[string]any{"path": "functions"}), `{
"title": "Functions", "url": "https://example.org/functions/", "kind": "section", "description": "Template functions.",
"entries": [
  {"title": "collections", "url": "https://example.org/functions/collections/", "kind": "section", "pages": 1},
  {"title": "strings", "url": "https://example.org/functions/strings/", "kind": "section", "pages": 1}
]}`)
	c.assertJSON(c.callOK("get_toc", map[string]any{"path": replaceURL}), `{
"title": "strings.Replace", "url": "https://example.org/functions/strings/replace/", "kind": "page",
"description": "Returns a copy of INPUT, replacing all occurrences of OLD with NEW.",
"headings": [
  {"level": 2, "heading": "Examples", "url": "https://example.org/functions/strings/replace/#examples"},
  {"level": 3, "heading": "Limit", "url": "https://example.org/functions/strings/replace/#limit"},
  {"level": 4, "heading": "Negative limit", "url": "https://example.org/functions/strings/replace/#negative"},
  {"level": 2, "heading": "See also", "url": "https://example.org/functions/strings/replace/#see-also"}
]}`)
	b.Assert(c.callErr("get_toc", map[string]any{"path": "/nope/"}), qt.Contains, `no page found for "/nope/".`)
	b.Assert(c.callErr("get_toc", map[string]any{"path": strings.Repeat("/a", 501)}), qt.Equals,
		"the path is too long, the maximum length is 1000 bytes")
}

// See issue 14747.
func TestVersionsAndLanguages(t *testing.T) {
	t.Parallel()
	files := `
-- hugo.toml --
baseURL = "https://example.org/"
title = "Docs"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
defaultContentLanguage = "en"
[languages.en]
weight = 1
[languages.nn]
weight = 2
title = "Dokumentasjon"
[versions."v1.0.0"]
[versions."v2.0.0"]
[[module.mounts]]
source = "content/v1"
target = "content"
[module.mounts.sites.matrix]
versions = ["v1.0.0"]
[[module.mounts]]
source = "content/v2"
target = "content"
[module.mounts.sites.matrix]
versions = ["v2.0.0"]
-- content/v1/install.md --
---
title: Install
---
Run the legacy installer.
-- content/v2/install.md --
---
title: Install
---
Run the new installer.
-- content/v2/install.nn.md --
---
title: Installasjon
---
Køyr det nye installasjonsprogrammet.
-- layouts/all.html --
{{ .Content }}
`
	b := hugolib.Test(t, files, hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	b.Assert(c.cs.InitializeResult().Instructions, qt.Contains,
		"Searches default to version v2.0.0, the available versions are v2.0.0, v1.0.0. "+
			"Searches default to language en, the available languages are en, nn.")

	tools, err := c.cs.ListTools(context.Background(), nil)
	b.Assert(err, qt.IsNil)
	b.Assert(tools.Tools, qt.HasLen, 3)
	for _, tool := range tools.Tools {
		b.Assert(tool.Annotations.ReadOnlyHint, qt.IsTrue)
		if tool.Name != "search_docs" {
			continue
		}
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		b.Assert(props["version"], qt.DeepEquals, map[string]any{
			"type": "string", "description": "The version to search.", "enum": []any{"v2.0.0", "v1.0.0"}, "default": "v2.0.0",
		})
		b.Assert(props["language"].(map[string]any)["enum"], qt.DeepEquals, []any{"en", "nn"})
	}

	b.Assert(c.search(map[string]any{"query": "installer"}), qt.DeepEquals, searchOutput{
		Version:  "v2.0.0",
		Language: "en",
		Results:  []searchResult{{Title: "Install", URL: "https://example.org/install/", Snippet: "Run the new installer."}},
	})
	b.Assert(c.search(map[string]any{"query": "installer", "version": "v1.0.0"}).Results, qt.DeepEquals,
		[]searchResult{{Title: "Install", URL: "https://example.org/v1.0.0/install/", Snippet: "Run the legacy installer."}})
	b.Assert(c.search(map[string]any{"query": "installasjonsprogrammet", "language": "nn"}).urls(), qt.DeepEquals,
		[]string{"https://example.org/nn/install/"})
	b.Assert(c.callErr("search_docs", map[string]any{"query": "installer", "version": "v9"}), qt.Contains, `validating "arguments"`)

	b.Assert(c.callOK("get_page", map[string]any{"path": "/install/"}), qt.Contains, "version: v2.0.0\nlanguage: en\n")
	b.Assert(c.callOK("get_page", map[string]any{"path": "https://example.org/nn/install/"}), qt.Contains,
		"version: v2.0.0\nlanguage: nn\n---\n\nKøyr det nye installasjonsprogrammet.\n")
	b.Assert(c.callOK("get_page", map[string]any{"path": "content/v1/install.md"}), qt.Contains, "Run the legacy installer.")

	var toc struct {
		Sites []map[string]any `json:"sites"`
	}
	b.Assert(json.Unmarshal([]byte(c.callOK("get_toc", nil)), &toc), qt.IsNil)
	b.Assert(toc.Sites, qt.HasLen, 4)
	b.Assert(toc.Sites[0], qt.DeepEquals, map[string]any{
		"title": "Docs", "url": "https://example.org/", "version": "v2.0.0", "language": "en", "default": true,
	})
}

// The index is updated after rebuilds, e.g. in watch mode.
// See issue 14747.
func TestUpdateAfterRebuild(t *testing.T) {
	t.Parallel()
	files := `
-- hugo.toml --
baseURL = "https://example.org/"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
-- content/_index.md --
---
title: Home
---
{{< pages >}}
-- content/p1.md --
---
title: Apple
---
The frobnicate setting.
-- layouts/_shortcodes/pages.html --
Pages: {{ range site.RegularPages }}{{ .Title }}, {{ end }}
-- layouts/all.html --
{{ .Content }}
`
	b := hugolib.Test(t, files, hugolib.TestOptRunning(), hugolib.TestOptSkipRender())
	c := newTestClient(t, b)

	b.Assert(c.search(map[string]any{"query": "frobnicate"}).urls(), qt.DeepEquals, []string{"https://example.org/p1/"})

	b.EditFileReplaceAll("content/p1.md", "frobnicate", "defenestrate").AddFiles("content/p2.md", "---\ntitle: Banana\n---\nYellow.").Build()
	c.update()
	b.Assert(c.search(map[string]any{"query": "frobnicate"}).Results, qt.HasLen, 0)
	b.Assert(c.search(map[string]any{"query": "defenestrate"}).urls(), qt.DeepEquals, []string{"https://example.org/p1/"})
	b.Assert(c.search(map[string]any{"query": "banana"}).urls(), qt.DeepEquals, []string{"https://example.org/p2/", "https://example.org/"})

	b.RemoveFiles("content/p1.md").Build()
	c.update()
	b.Assert(c.callErr("get_page", map[string]any{"path": "/p1/"}), qt.Contains, `no page found for "/p1/".`)
	b.Assert(c.callOK("get_page", map[string]any{"path": "/"}), qt.Contains, "Pages: Banana,\n")

	// A failed build keeps the last index, with a warning until the next successful build.
	const warning = "The last build failed, so the content may be outdated: boom"
	c.s.SetError(errors.New("boom"))
	out := c.search(map[string]any{"query": "banana"})
	b.Assert(out.Results, qt.HasLen, 2)
	b.Assert(out.Warning, qt.Equals, warning)
	b.Assert(c.callOK("get_page", map[string]any{"path": "/p2/"}), qt.Matches, `(?s)---\n.*warning: .?`+warning+`.*Yellow\..*`)
	b.Assert(c.callOK("get_toc", nil), qt.Contains, `"warning":"`+warning+`"`)
	b.Assert(c.callErr("get_page", map[string]any{"path": "/p3/"}), qt.Matches, `no page found for "/p3/"\..* `+warning)

	c.update()
	b.Assert(c.search(map[string]any{"query": "banana"}).Warning, qt.Equals, "")
	b.Assert(c.callOK("get_page", map[string]any{"path": "/p2/"}), qt.Not(qt.Contains), "warning:")
}

// Tool calls wait for the first build to be indexed.
// See issue 14747.
func TestToolCallsWaitForIndex(t *testing.T) {
	t.Parallel()
	b := hugolib.Test(t, basicSite, hugolib.TestOptSkipRender())
	c := connect(t, b)

	done := make(chan error)
	go func() {
		res, err := c.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_toc"})
		if err == nil && res.IsError {
			err = errors.New("tool error")
		}
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("the tool call returned before the site was indexed")
	case <-time.After(100 * time.Millisecond):
	}
	c.update()
	b.Assert(<-done, qt.IsNil)
}

// Tool calls return the build error until the site is indexed.
// See issue 14747.
func TestToolCallsReturnBuildError(t *testing.T) {
	t.Parallel()
	b := hugolib.Test(t, basicSite, hugolib.TestOptSkipRender())
	c := connect(t, b)

	c.s.SetError(errors.New("boom"))
	b.Assert(c.callErr("get_toc", nil), qt.Equals, "the site failed to build: boom")
	c.update()
	c.callOK("get_toc", nil)
}
