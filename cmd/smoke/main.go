// Command smoke is a live end-to-end check: it starts the server over stdio
// and calls real tools. Run it from the repository root with a token in place:
//
//	go run ./cmd/smoke
//
// The `// Command smoke ...` comment above is a package comment for a main
// package, which is how Go documents binaries. `go doc ./cmd/smoke` shows it.
package main

import (
	"context"
	"fmt"
	"log"
	// os/exec launches child processes. The MCP client needs it because a
	// local MCP server is just a subprocess speaking a protocol over pipes.
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// main of a separate command. This directory builds its own binary; it is not
// part of the server binary in the repository root. A Go project can hold many
// main packages, one per command, and `go run ./cmd/smoke` picks this one.
func main() {
	// Same root context as the server; nothing here needs cancellation.
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "hardcover-smoke", Version: "0.1.0"}, nil)

	// CommandTransport starts the given command and talks MCP to it over
	// stdin/stdout. exec.Command("go", "run", ".") compiles and runs the server
	// from the current directory, so this must be run from the repo root.
	//
	// The child inherits this process's environment, including
	// HARDCOVER_TOKEN_PATH, which is why no explicit env passing is needed
	// here (the Python smoke test had to forward it by hand).
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command("go", "run", ".")}, nil)
	if err != nil {
		log.Fatal(err)
	}
	// Close the session and the child process when main returns.
	defer session.Close()

	// ListTools asks the server what it provides: the same call an MCP client
	// makes to build its tool menu.
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	// make with an exact capacity, then append. Names only, for printing.
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	fmt.Println("tools:", names)

	// An anonymous struct slice is a compact table: each element is one call.
	// Field names are lower-case because the struct is local, not JSON-decoded.
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		// nil arguments for a tool that takes none.
		{"hardcover_stats", nil},
		{"hardcover_library", map[string]any{"status": "read", "limit": 3}},
		// A non-ASCII query proves Unicode survives the JSON and stdio layers.
		{"hardcover_search", map[string]any{"query": "Мастер и Маргарита"}},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			log.Fatalf("%s: %v", call.name, err)
		}
		fmt.Printf("\n%s:\n", call.name)
		// Content is a slice of the mcp.Content interface. A type switch or,
		// as here, a type assertion with comma-ok extracts the text blocks;
		// images and embedded resources are other implementations.
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				fmt.Println(text.Text)
			}
		}
		// IsError is the MCP-level flag for a failed tool call; the Go error
		// from CallTool only covers protocol failures.
		if result.IsError {
			log.Fatalf("%s failed", call.name)
		}
	}
}
