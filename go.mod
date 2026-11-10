// The module path is the import prefix for every package in this repo. It
// matches the GitHub remote so the module could be fetched by path; nothing
// else in the project depends on that, the local build works regardless.
module github.com/rhrabun/hardcover-mcp

// Minimum Go version. The official MCP SDK requires 1.25. This is a language
// requirement, not a pin on the installed toolchain: Go 1.27 builds it fine.
go 1.25.0

// Direct dependency: the official MCP Go SDK. Only its /mcp package is
// imported, which is why the import lines read
// "github.com/modelcontextprotocol/go-sdk/mcp".
require github.com/modelcontextprotocol/go-sdk v1.8.0

// Indirect dependencies are pulled in by the SDK and its own imports. `go mod
// tidy` maintains this block (and go.sum); it is not edited by hand.
require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
