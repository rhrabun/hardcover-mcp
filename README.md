# hardcover-mcp

An MCP server that exposes a Hardcover library to an agent: read books, ratings,
catalogue search and write access. Hardcover's GraphQL API is used directly.

The server is written in Go with the
[official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk). The original
Python implementation is kept in [`python/`](python/) as a reference; both expose
the same six tools.

## Credentials

- `HARDCOVER_TOKEN_PATH` (default `~/.config/hardcover/token`) - path to a file holding
  the API token, sent as `Authorization: Bearer <token>`. The file is read on every
  call, so a rotated token needs no restart.

## Tools

| Tool | Purpose |
|------|---------|
| `hardcover_search` | Find books by title or author; Unicode (e.g. Cyrillic) queries work |
| `hardcover_library` | Shelf contents by status: read / reading / want / dnf (defaults to read) |
| `hardcover_mark_read` | Mark read, with optional rating and finish date (no date = no date) |
| `hardcover_set_rating` | Set or change a rating (0.5-5) on a book already on a shelf |
| `hardcover_set_status` | Move a book between shelves |
| `hardcover_stats` | Counts and the size of the missing-date gap |

Tools return typed JSON in `structuredContent` as well as text content. A failed
call sets `isError`, with the reason in the text content.

## Three Hardcover quirks this server hides

1. **The finished date is dropped when a read record is first created.** `mark_read` writes
   the date in a follow-up call and reads it back, because a success response does not carry
   the value.
2. **Inserting a book as read stamps a read row dated Hardcover's own today.** Marking a book
   read without a finish date deletes that row, so the book ends up read with no date instead
   of carrying a date the caller never supplied.
3. **Book titles are English by Hardcover's policy**, even for non-English books. Searches
   match the caller's wording, and writes report the record they matched.

## Setup

Requires Go 1.25 or newer.

```sh
go build -o hardcover-mcp .   # the server binary
go test ./...                 # self-check against a stubbed GraphQL client
go run ./cmd/smoke            # live end-to-end check over stdio, run from the repo root
```

## Registration

```json
{
  "mcpServers": {
    "hardcover": {
      "command": "/path/to/hardcover-mcp",
      "args": [],
      "env": { "HARDCOVER_TOKEN_PATH": "/path/to/hardcover.token" }
    }
  }
}
```

## Python reference

`python/` holds the previous implementation, same tools and behaviour.

Requires [uv](https://docs.astral.sh/uv/).

```sh
cd python
uv sync
uv run python test_server.py   # self-check on the pure helpers
uv run python smoke.py         # live end-to-end check over stdio
```

To register it instead:

```json
{
  "mcpServers": {
    "hardcover": {
      "command": "uv",
      "args": ["run", "--directory", "/path/to/hardcover-mcp/python", "python", "server.py"],
      "env": { "HARDCOVER_TOKEN_PATH": "/path/to/hardcover.token" }
    }
  }
}
```
