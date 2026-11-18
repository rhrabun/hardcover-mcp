package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "hardcover", Version: "0.1.0"}, nil)
	client, err := NewClient()
	if err != nil {
		log.Fatal(err)
	}
	addTools(server, client)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
