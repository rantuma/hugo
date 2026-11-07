---
title: MCP server
description: Use the hugo mcp command to let AI agents search and read the content of your project.
categories: []
keywords: []
weight: 15
---

{{< new-in 0.170.0 />}}

The `hugo mcp` command starts a [Model Context Protocol][] (MCP) server that AI agents use to search and read the content of your project. Use it to give a coding assistant accurate and current information from your documentation, or from the Hugo documentation itself.

The server builds your project in memory, splits the content of each page at its headings, and indexes the sections for keyword search. It provides these read-only tools:

`search_docs`
: Searches the content and returns the best matching sections. Each result includes the page title, the headings leading to the section, the URL with an anchor, and a snippet. Exact names of functions, methods, configuration settings, and error messages work best, but keywords and questions work too.

`get_page`
: Returns a page, or one section of it, as Markdown. A YAML header includes the title, URL, description, last modified date, and front matter parameters of the page. The server splits long pages into parts.

`get_toc`
: Lists the sections and pages of the project, and the headings of a page. Agents use it to browse when they don't know what to search for.

If your project has more than one [version](g) or [language](g), the tools use the [default version](g) and the [default language](g) unless the agent asks for another one.

## Configure your client

Most clients start the server for you and communicate with it over standard input and output. Configure your client to run `hugo mcp` with the `--source` flag set to the project directory. Many clients use this format:

```json
{
  "mcpServers": {
    "hugo": {
      "command": "hugo",
      "args": ["mcp", "--source", "/path/to/project"]
    }
  }
}
```

In Visual Studio Code, add the server to the `.vscode/mcp.json` file in your project:

```json {file=".vscode/mcp.json"}
{
  "servers": {
    "hugo": {
      "type": "stdio",
      "command": "hugo",
      "args": ["mcp", "--source", "${workspaceFolder}"]
    }
  }
}
```

In Claude Code, add the server with the `claude mcp add` command:

```sh
claude mcp add hugo -- hugo mcp --source /path/to/project
```

Like the `hugo build` command, the server excludes draft, future, and expired content unless you add the `--buildDrafts`, `--buildFuture`, or `--buildExpired` flag. Add the `--watch` flag to rebuild and reindex the project when you change its files. If a rebuild fails, the tools return the content from the last successful build with a warning that includes the error.

## Search the Hugo documentation

To give your agent accurate and current information about Hugo, clone the [documentation repository][]:

```sh
git clone https://github.com/gohugoio/hugoDocs.git
```

Then configure your client to run `hugo mcp --source /path/to/hugoDocs`.

## Serve over HTTP

Use the `--http` flag to serve the [Streamable HTTP][] transport at the `/mcp` path on the given address, instead of standard input and output. Use it to share one server between clients, or to host a server for your published site:

```sh
hugo mcp --http localhost:1314
```

Then configure your client to connect to `http://localhost:1314/mcp`.

The server does not authenticate clients, so bind it to a public address only if the content of your project is public. To protect against [DNS rebinding][] attacks, the server rejects requests to a loopback address unless the Host header is `localhost` or a loopback address. If you run a reverse proxy on the same machine, configure it to send such a Host header.

The server builds the project when it starts. When you host a server for your published site, deploy it from the same source as the site, so that the URLs in the search results point to existing pages.

[DNS rebinding]: https://en.wikipedia.org/wiki/DNS_rebinding
[documentation repository]: https://github.com/gohugoio/hugoDocs
[Model Context Protocol]: https://modelcontextprotocol.io/
[Streamable HTTP]: https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http
