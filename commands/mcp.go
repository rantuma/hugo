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

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bep/simplecobra"
	"github.com/gohugoio/hugo/common/hstrings"
	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/hugolib"
	"github.com/gohugoio/hugo/internal/mcpserver"
	"github.com/gohugoio/hugo/source"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func newMCPCommand() *mcpCommand {
	return &mcpCommand{}
}

type mcpCommand struct {
	r *rootCommand
	*hugoBuilder

	httpAddr string
	watch    bool
}

func (c *mcpCommand) Commands() []simplecobra.Commander {
	return nil
}

func (c *mcpCommand) Name() string {
	return "mcp"
}

func (c *mcpCommand) Init(cd *simplecobra.Commandeer) error {
	cmd := cd.CobraCommand
	cmd.Short = "Start an MCP server to search and read your project"
	cmd.Long = `Start a Model Context Protocol (MCP) server that AI agents can use to search and read
the content of your project.

The server builds the project in memory, splits the content of its pages at the headings,
and indexes the sections for search. It provides these read-only tools:

- search_docs: search the content and return the best matching sections with their URLs
- get_page: get a page, or a section of it, as Markdown
- get_toc: browse the sections and pages of the project

By default, the MCP client starts the server and communicates with it over stdin and stdout.
Use ` + "`--http`" + ` to serve the Streamable HTTP transport at /mcp on the given address instead.

Use ` + "`--watch`" + ` to rebuild and reindex the project when its files change.`
	cmd.Args = cobra.NoArgs
	cmd.Flags().StringVar(&c.httpAddr, "http", "", "serve the Streamable HTTP transport on this address, e.g. localhost:1314, instead of stdio")
	_ = cmd.RegisterFlagCompletionFunc("http", cobra.NoFileCompletions)
	cmd.Flags().BoolVarP(&c.watch, "watch", "w", false, "watch filesystem for changes and reindex as needed")
	applyLocalFlagsBuild(cmd, cd.Root.Command.(*rootCommand))
	return nil
}

func (c *mcpCommand) PreRun(cd, runner *simplecobra.Commandeer) error {
	c.r = cd.Root.Command.(*rootCommand)
	c.r.renderToMemory = true
	c.r.buildWatch = c.watch
	c.hugoBuilder = newHugoBuilder(c.r, nil)
	c.skipRender = true
	return c.loadConfig(cd, false)
}

func (c *mcpCommand) Run(ctx context.Context, cd *simplecobra.Commandeer, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var srv *mcpserver.Server
	c.withConf(func(conf *commonConfig) {
		srv = mcpserver.New(conf.configs, c.r.logger)
	})
	c.onBuildDone = func(h *hugolib.HugoSites, err error) error {
		if err != nil {
			srv.SetError(err)
			return err
		}
		return srv.Update(ctx, h)
	}

	if c.watch {
		spec, err := c.sourceSpec()
		if err != nil {
			return err
		}
		watchDirs := hstrings.UniqueStringsSorted(spec.BaseFs.WatchFilenames())
		c.r.Printf("Watching for changes in %s\n", strings.Join(helpers.ExtractAndGroupRootPaths(watchDirs), ", "))
		watcher, err := c.newSourceWatcher(spec, c.r.poll, watchDirs...)
		if err != nil {
			return err
		}
		defer watcher.Close()
	}

	stopProfiling, err := c.initProfiling()
	if err != nil {
		return err
	}
	// Stop on exit if the build is still running.
	stopProfiling = sync.OnceFunc(stopProfiling)
	defer stopProfiling()
	go func() {
		defer stopProfiling()
		start := time.Now()
		if err := c.buildSites(false); err != nil {
			c.handleBuildErr(err, "Build failed")
			return
		}
		c.r.timeTrack(start, "Built and indexed")
	}()

	if c.httpAddr == "" {
		err := srv.Run(ctx, &mcp.StdioTransport{})
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return c.serveHTTP(ctx, srv)
}

// sourceSpec returns the source spec of the sites or, if they can't be created,
// e.g. because of a template error, one created from the config,
// so we can watch for the fix and serve the error until then.
func (c *mcpCommand) sourceSpec() (*source.SourceSpec, error) {
	h, err := c.hugo()
	if err == nil {
		return h.Deps.SourceSpec, nil
	}
	var spec *source.SourceSpec
	err = c.withConfE(func(conf *commonConfig) error {
		ps, err := helpers.NewPathSpec(conf.fs, conf.configs.GetFirstLanguageConfig(), c.r.logger, nil)
		if err != nil {
			return err
		}
		spec = source.NewSourceSpec(ps, nil, conf.fs.Source)
		return nil
	})
	return spec, err
}

func (c *mcpCommand) serveHTTP(ctx context.Context, srv *mcpserver.Server) error {
	ln, err := net.Listen("tcp", c.httpAddr)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	mux := http.NewServeMux()
	mux.Handle("/mcp", srv.HTTPHandler())
	if c.r.IsTestRun() {
		mux.HandleFunc("/__stop", func(http.ResponseWriter, *http.Request) { stop() })
	}
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		errc <- hs.Serve(ln)
	}()

	addr := ln.Addr().(*net.TCPAddr)
	host := "localhost"
	if !addr.IP.IsUnspecified() {
		host = addr.IP.String()
	}
	baseURL := "http://" + net.JoinHostPort(host, strconv.Itoa(addr.Port)) + "/"
	c.r.Printf("MCP server is available at %smcp\n", baseURL)
	c.r.Println("Press Ctrl+C to stop")

	if dir := os.Getenv("WORK"); c.r.IsTestRun() && dir != "" {
		// Signal to the test that the server is ready.
		b, err := json.Marshal(map[string]any{"baseURLs": []string{baseURL}})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, ".ready"), b, 0o666); err != nil {
			return err
		}
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
