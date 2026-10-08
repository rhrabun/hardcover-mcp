# hardcover-mcp

An MCP server that exposes a Hardcover library to an agent: read books, ratings,
catalogue search and write access. Hardcover's GraphQL API is used directly.

## Credentials

- `HARDCOVER_TOKEN_PATH` (default `hardcover.token`) - path to a file holding the API
  token, sent as `Authorization: Bearer <token>`.

## Tools

| Tool | Purpose |
|------|---------|
| `hardcover_search` | Find books by title or author; Unicode (e.g. Cyrillic) queries work |
| `hardcover_library` | Shelf contents by status: read / reading / want / dnf (defaults to read) |
| `hardcover_mark_read` | Mark read, with optional rating and finish date |
| `hardcover_set_rating` | Set or change a rating (0.5-5) |
| `hardcover_set_status` | Move a book between shelves |
| `hardcover_stats` | Counts and the size of the missing-date gap |

## Two Hardcover quirks this server hides

1. **The finished date is dropped when a read record is first created.** `mark_read` writes
   the date in a follow-up call, which is what actually populates `last_read_date`.
2. **Book titles are English by Hardcover's policy**, even for non-English books. Searches
   match the caller's wording, and writes report the record they matched.

## Setup

Requires [uv](https://docs.astral.sh/uv/).

```sh
uv sync
uv run python test_server.py   # self-check on the pure helpers
uv run python smoke.py         # live end-to-end check over stdio
```

## Registration

```json
{
  "mcpServers": {
    "hardcover": {
      "command": "uv",
      "args": ["run", "--directory", "/path/to/hardcover-mcp", "python", "server.py"],
      "env": { "HARDCOVER_TOKEN_PATH": "/path/to/hardcover.token" }
    }
  }
}
```
