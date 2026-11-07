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

// Package mcpserver provides a Model Context Protocol (MCP) server
// to search and read the content of a Hugo site.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/gohugoio/hugo/common/hugo"
	"github.com/gohugoio/hugo/common/loggers"
	"github.com/gohugoio/hugo/config/allconfig"
	"github.com/gohugoio/hugo/hugolib"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultSearchLimit = 5
	maxSearchLimit     = 20

	// maxInputLen is the maximum length in bytes of the query and path inputs.
	maxInputLen = 1000

	// maxWarningLen is the maximum length in bytes of the build error in warnings.
	maxWarningLen = 1000
)

// Server is an MCP server to search and read the content of a Hugo site.
// The content is indexed with Update after each build.
type Server struct {
	logger   loggers.Logger
	conv     *htmltomarkdown.Converter
	server   *mcp.Server
	siteName string

	mu        sync.Mutex // Serializes updates.
	state     atomic.Pointer[state]
	ready     chan struct{}
	readyOnce sync.Once

	// The versions and languages in the registered search_docs schema.
	versions, languages dimension
}

type state struct {
	idx *index
	err error // The error from the last build, if any.
}

// warning returns a warning that the content may be outdated if the last build failed.
func (st *state) warning() string {
	if st.err == nil {
		return ""
	}
	return "The last build failed, so the content may be outdated: " + truncate(st.err.Error(), maxWarningLen)
}

// withWarning adds the warning, if any, to err, e.g. a page that's not found in an outdated index.
func (st *state) withWarning(err error) error {
	if w := st.warning(); w != "" {
		return fmt.Errorf("%w %s", err, w)
	}
	return err
}

// dimension is the available values of e.g. the version of the sites.
type dimension struct {
	values []string
	def    string
}

func (d dimension) equal(o dimension) bool {
	return d.def == o.def && slices.Equal(d.values, o.values)
}

// New creates a new MCP server for the site with the given configuration.
// Tool calls wait until the first call to Update or SetError.
func New(conf *allconfig.Configs, logger loggers.Logger) *Server {
	var versions dimension
	for _, v := range conf.Base.Versions.Config.Sorted {
		versions.values = append(versions.values, v.Name)
		if v.Default {
			versions.def = v.Name
		}
	}
	languages := dimension{def: conf.Base.DefaultContentLanguage}
	for _, l := range conf.Languages {
		languages.values = append(languages.values, l.Lang)
	}

	s := &Server{
		logger:   logger,
		conv:     newMarkdownConverter(),
		siteName: siteName(conf.Base.Title, conf.Base.BaseURL),
		ready:    make(chan struct{}),
	}
	s.server = mcp.NewServer(&mcp.Implementation{
		Name:       "hugo",
		Title:      "Hugo",
		Version:    hugo.CurrentVersion.String(),
		WebsiteURL: "https://gohugo.io/",
	}, &mcp.ServerOptions{
		Instructions: s.instructions(versions, languages),
	})
	s.addSearchTool(versions, languages)
	mcp.AddTool(s.server, &mcp.Tool{
		Name:  "get_page",
		Title: "Get page",
		Description: fmt.Sprintf("Get a page of %s, or a section of it, as Markdown. "+
			"Pass a url from search_docs or get_toc, or a path. "+
			"A URL with an #anchor, or the heading argument, returns just that section including its subsections, or that definition list entry. "+
			"The YAML header has the title, URL, description, last modified date and front matter params of the page. "+
			"Long pages are split into parts; the footer tells how to get the next part.", s.siteName),
		InputSchema: pageSchema(),
		Annotations: readOnlyAnnotations("Get page"),
	}, s.getPage)
	mcp.AddTool(s.server, &mcp.Tool{
		Name:  "get_toc",
		Title: "Get table of contents",
		Description: fmt.Sprintf("Browse the structure of %s. "+
			"Without a path, lists the top level sections and pages, and the available versions and languages. "+
			"With the URL or path of a section, lists its pages and subsections. "+
			"Also lists the headings of the page. "+
			"Use it when you don't know what to search for.", s.siteName),
		Annotations: readOnlyAnnotations("Get table of contents"),
	}, s.getTOC)
	return s
}

// Update indexes the content of the sites in h after a build,
// reusing the content of unchanged pages from the previous index.
// It must not run concurrently with a build of h.
func (s *Server) Update(ctx context.Context, h *hugolib.HugoSites) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev *index
	if st := s.state.Load(); st != nil {
		prev = st.idx
	}
	idx, err := newIndex(ctx, h, prev, s.conv, s.logger)
	if err != nil {
		s.setErrorLocked(err)
		return err
	}
	s.state.Store(&state{idx: idx})
	def := idx.sites[idx.defaultSite]
	versions := dimension{values: idx.versions, def: def.version}
	languages := dimension{values: idx.languages, def: def.language}
	if !versions.equal(s.versions) || !languages.equal(s.languages) {
		s.addSearchTool(versions, languages)
	}
	s.readyOnce.Do(func() { close(s.ready) })
	return nil
}

// SetError records a failed build.
// The tools keep using the last index, or return err if there is none.
func (s *Server) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setErrorLocked(err)
}

func (s *Server) setErrorLocked(err error) {
	var prev *index
	if st := s.state.Load(); st != nil {
		prev = st.idx
	}
	s.state.Store(&state{idx: prev, err: err})
	s.readyOnce.Do(func() { close(s.ready) })
}

// Run runs the server over the given transport until the client disconnects or ctx is done.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	return s.server.Run(ctx, t)
}

// HTTPHandler returns a handler that serves the MCP server with the stateless Streamable HTTP transport.
func (s *Server) HTTPHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	return http.NewCrossOriginProtection().Handler(h)
}

// currentState waits for the first build and returns the current state, which has an index.
func (s *Server) currentState(ctx context.Context) (*state, error) {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	st := s.state.Load()
	if st.idx == nil {
		return nil, fmt.Errorf("the site failed to build: %w", st.err)
	}
	return st, nil
}

func (s *Server) instructions(versions, languages dimension) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Search and read the documentation and other content of %s, built with Hugo. ", s.siteName)
	sb.WriteString("Use search_docs to find the relevant sections, then get_page with a result's url to read it as Markdown. ")
	sb.WriteString("Use get_toc to browse the site structure.")
	if len(versions.values) > 1 {
		fmt.Fprintf(&sb, " Searches default to version %s, the available versions are %s.", versions.def, strings.Join(versions.values, ", "))
	}
	if len(languages.values) > 1 {
		fmt.Fprintf(&sb, " Searches default to language %s, the available languages are %s.", languages.def, strings.Join(languages.values, ", "))
	}
	return sb.String()
}

func siteName(title, baseURL string) string {
	switch {
	case title != "" && baseURL != "":
		return fmt.Sprintf("%s (%s)", title, baseURL)
	case title != "":
		return title
	case baseURL != "":
		return baseURL
	}
	return "the site"
}

func readOnlyAnnotations(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)}
}

type searchInput struct {
	Query    string `json:"query" jsonschema:"What to search for: an exact name (e.g. of a function, method, configuration setting or error message), keywords or a question."`
	Version  string `json:"version,omitempty" jsonschema:"The version to search."`
	Language string `json:"language,omitempty" jsonschema:"The language to search."`
	Section  string `json:"section,omitempty" jsonschema:"Only search in this top level section or below this path."`
	Limit    int    `json:"limit,omitempty" jsonschema:"The maximum number of results."`
}

type searchOutput struct {
	Version  string         `json:"version,omitempty" jsonschema:"The version searched."`
	Language string         `json:"language,omitempty" jsonschema:"The language searched."`
	Results  []searchResult `json:"results"`
	Note     string         `json:"note,omitempty"`
	Warning  string         `json:"warning,omitempty" jsonschema:"Set if the content may be outdated because the last build failed."`
}

type pageInput struct {
	Path    string `json:"path" jsonschema:"The URL or path of the page, e.g. a url from search_docs. A #anchor selects a section of the page."`
	Heading string `json:"heading,omitempty" jsonschema:"Get only the section with this heading anchor or text, including its subsections."`
	Part    int    `json:"part,omitempty" jsonschema:"The part to get of a page that is split into parts."`
}

type tocInput struct {
	Path string `json:"path,omitempty" jsonschema:"The URL or path of a section or page. Omit to start at the home page."`
}

// addSearchTool adds or replaces the search_docs tool.
func (s *Server) addSearchTool(versions, languages dimension) {
	s.versions, s.languages = versions, languages
	mcp.AddTool(s.server, &mcp.Tool{
		Name:  "search_docs",
		Title: "Search documentation",
		Description: fmt.Sprintf("Search the documentation and other content of %s. "+
			"Returns the best matching sections of pages, each with the page title, the headings leading to the section, "+
			"the URL (with an #anchor for sections) and a snippet. "+
			"Exact names work best, e.g. of functions, methods, configuration settings and error messages, "+
			"but keywords and questions work too. Read a result in full with get_page.", s.siteName),
		InputSchema: searchSchema(versions, languages),
		Annotations: readOnlyAnnotations("Search documentation"),
	}, s.searchDocs)
}

func searchSchema(versions, languages dimension) *jsonschema.Schema {
	schema := schemaFor[searchInput]()
	limit := schema.Properties["limit"]
	limit.Default = json.RawMessage(strconv.Itoa(defaultSearchLimit))
	limit.Minimum = new(1.0)
	limit.Maximum = new(float64(maxSearchLimit))
	setEnum(schema, "version", versions)
	setEnum(schema, "language", languages)
	return schema
}

func pageSchema() *jsonschema.Schema {
	schema := schemaFor[pageInput]()
	part := schema.Properties["part"]
	part.Default = json.RawMessage("1")
	part.Minimum = new(1.0)
	return schema
}

// setEnum limits the property name to the values in d, or removes it if there is only one value.
func setEnum(schema *jsonschema.Schema, name string, d dimension) {
	if len(d.values) < 2 {
		delete(schema.Properties, name)
		schema.PropertyOrder = slices.DeleteFunc(schema.PropertyOrder, func(s string) bool { return s == name })
		return
	}
	p := schema.Properties[name]
	for _, v := range d.values {
		p.Enum = append(p.Enum, v)
	}
	p.Default, _ = json.Marshal(d.def)
}

func schemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	return schema
}

func (s *Server) searchDocs(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	if err := checkInputLen("query", in.Query); err != nil {
		return nil, searchOutput{}, err
	}
	st, err := s.currentState(ctx)
	if err != nil {
		return nil, searchOutput{}, err
	}
	ix := st.idx
	limit := in.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	site, hits, err := ix.search(searchOptions{
		query:    in.Query,
		version:  in.Version,
		language: in.Language,
		section:  in.Section,
		limit:    min(limit, maxSearchLimit),
	})
	if err != nil {
		return nil, searchOutput{}, st.withWarning(err)
	}
	out := searchOutput{Results: ix.searchResults(site, hits, in.Query), Warning: st.warning()}
	if len(ix.versions) > 1 {
		out.Version = site.version
	}
	if len(ix.languages) > 1 {
		out.Language = site.language
	}
	if len(hits) == 0 {
		out.Note = "No matches. Try other or fewer words, or browse with get_toc."
	}
	return nil, out, nil
}

func (s *Server) getPage(ctx context.Context, _ *mcp.CallToolRequest, in pageInput) (*mcp.CallToolResult, any, error) {
	if err := checkInputLen("path", in.Path); err != nil {
		return nil, nil, err
	}
	st, err := s.currentState(ctx)
	if err != nil {
		return nil, nil, err
	}
	ix := st.idx
	pi, fragment, found := ix.find(in.Path)
	if !found {
		return nil, nil, st.withWarning(ix.notFoundError(in.Path))
	}
	md, err := ix.renderPage(pageRequest{page: pi, heading: in.Heading, fragment: fragment, part: in.Part, warning: st.warning()}, maxPartSize)
	if err != nil {
		return nil, nil, st.withWarning(err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: md}}}, nil, nil
}

func (s *Server) getTOC(ctx context.Context, _ *mcp.CallToolRequest, in tocInput) (*mcp.CallToolResult, tocOutput, error) {
	if err := checkInputLen("path", in.Path); err != nil {
		return nil, tocOutput{}, err
	}
	st, err := s.currentState(ctx)
	if err != nil {
		return nil, tocOutput{}, err
	}
	ix := st.idx
	pi := ix.sites[ix.defaultSite].home
	if strings.TrimSpace(in.Path) != "" {
		var found bool
		if pi, _, found = ix.find(in.Path); !found {
			return nil, tocOutput{}, st.withWarning(ix.notFoundError(in.Path))
		}
	}
	if pi == -1 {
		return nil, tocOutput{}, st.withWarning(errors.New("the site has no home page, use search_docs to find pages"))
	}
	out := ix.toc(pi)
	out.Warning = st.warning()
	return nil, out, nil
}

// checkInputLen bounds the cost of the input, without echoing it in the error.
func checkInputLen(name, s string) error {
	if len(s) > maxInputLen {
		return fmt.Errorf("the %s is too long, the maximum length is %d bytes", name, maxInputLen)
	}
	return nil
}
