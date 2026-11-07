---
title: "hugo mcp"
slug: hugo_mcp
url: /commands/hugo_mcp/
---
## hugo mcp

Start an MCP server to search and read your project

### Synopsis

Start a Model Context Protocol (MCP) server that AI agents can use to search and read
the content of your project.

The server builds the project in memory, splits the content of its pages at the headings,
and indexes the sections for search. It provides these read-only tools:

- search_docs: search the content and return the best matching sections with their URLs
- get_page: get a page, or a section of it, as Markdown
- get_toc: browse the sections and pages of the project

By default, the MCP client starts the server and communicates with it over stdin and stdout.
Use `--http` to serve the Streamable HTTP transport at /mcp on the given address instead.

Use `--watch` to rebuild and reindex the project when its files change.

```
hugo mcp [flags] [args]
```

### Options

```
  -b, --baseURL string           hostname (and path) to the root, e.g. https://spf13.com/
  -D, --buildDrafts              include content marked as draft
  -E, --buildExpired             include expired content
  -F, --buildFuture              include content with publishdate in the future
      --cacheDir string          filesystem path to cache directory
      --cleanDestinationDir      remove stale files from destination
  -c, --contentDir string        filesystem path to content directory
      --disableKinds strings     disable different kind of pages (home, RSS etc.)
      --enableGitInfo            add Git revision, date, author, and CODEOWNERS info to the pages
      --forceSyncStatic          copy all files when static is changed.
      --gc                       enable to run some cleanup tasks (remove unused cache files) after the build
  -h, --help                     help for mcp
      --http string              serve the Streamable HTTP transport on this address, e.g. localhost:1314, instead of stdio
      --ignoreCache              ignore the configured file caches
  -l, --layoutDir string         filesystem path to layout directory
      --minify                   minify any supported output format (HTML, XML etc.)
      --noChmod                  don't sync permission mode of files
      --noTimes                  don't sync modification time of files
      --panicOnWarning           panic on first WARNING log
      --poll string              set this to a poll interval, e.g --poll 700ms, to use a poll based approach to watch for file system changes
      --printI18nWarnings        print missing translations
      --printMemoryUsage         print memory usage to screen at intervals
      --printPathWarnings        print warnings on duplicate target paths etc.
      --printUnusedTemplates     print warnings on unused templates.
      --renderSegments strings   named segments to render (configured in the segments config)
      --templateMetrics          display metrics about template executions
      --templateMetricsHints     calculate some improvement hints when combined with --templateMetrics
  -t, --theme strings            themes to use (located in /themes/THEMENAME/)
      --trace file               write trace to file (not useful in general)
  -w, --watch                    watch filesystem for changes and reindex as needed
```

### Options inherited from parent commands

```
      --clock string               set the clock used by Hugo, e.g. --clock 2021-11-06T22:30:00.00+09:00
      --config string              config file (default is hugo.yaml|json|toml)
      --configDir string           config dir (default "config")
  -d, --destination string         filesystem path to write files to
  -e, --environment string         build environment
      --ignoreVendorPaths string   ignores any _vendor for module paths matching the given Glob pattern
      --logLevel string            log level (debug|info|warn|error)
      --noBuildLock                don't create .hugo_build.lock file
      --quiet                      build in quiet mode
  -M, --renderToMemory             render to memory (mostly useful when running the server)
  -s, --source string              filesystem path to read files relative from
      --themesDir string           filesystem path to themes directory
```

### SEE ALSO

* [hugo](/commands/hugo/)	 - Build your project
