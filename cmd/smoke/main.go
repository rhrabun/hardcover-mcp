// Command smoke is a live end-to-end check: it starts the server over stdio
// and calls real tools. Run it from the repository root with a token in place.
package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "hardcover-smoke", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command("go", "run", ".")}, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	fmt.Println("tools:", names)

	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"hardcover_stats", nil},
		{"hardcover_library", map[string]any{"status": "read", "limit": 3}},
		{"hardcover_search", map[string]any{"query": "Мастер и Маргарита"}},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			log.Fatalf("%s: %v", call.name, err)
		}
		fmt.Printf("\n%s:\n", call.name)
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				fmt.Println(text.Text)
			}
		}
		if result.IsError {
			log.Fatalf("%s failed", call.name)
		}
	}
}
