// Every Go program starts in package main and in func main in this file.
// Without both, `go build .` fails with "function main is undeclared".
package main

// Imports are per file, not per project. If a name is used in this file, its
// package must be imported here, even when another file in the same package
// already imports it.
import (
	// context.Background() below is the root context: a value that is never
	// cancelled and carries no deadline. It is passed explicitly to everything
	// that might block, which is how Go does cancellation and timeouts without
	// threads or cancellation tokens.
	"context"

	// log.Fatal prints to stderr and exits with status 1. Using the standard
	// logger keeps stdout clean for the MCP protocol stream.
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// NewServer builds the MCP server. Implementation is the name/version the
	// client sees during initialization; nil means "default options".
	//
	// The type here is *mcp.Server (a pointer), and addTools takes that pointer
	// so it can register tools on the same instance.
	server := mcp.NewServer(&mcp.Implementation{Name: "hardcover", Version: "0.1.0"}, nil)

	// NewClient returns the Hardcover client, and addTools registers all six
	// tools as methods bound to it. The client holds no network connection yet;
	// it only reads the token when a tool is actually called.
	addTools(server, NewClient())

	// StdioTransport speaks MCP over stdin/stdout, which is what MCP clients
	// launch for a local server. Run blocks until the client disconnects or the
	// process is killed.
	//
	// server.Run returns an error, and `if err := ...; err != nil` is the
	// standard "handle it immediately" pattern. log.Fatal is acceptable at
	// startup, where there is nothing left to do.
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
