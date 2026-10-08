"""MCP server for Hardcover: read books, ratings, search, writes.

Two Hardcover quirks are handled here so callers never hit them:
  - the finished date is silently dropped when a read record is first created, so it is
    written in a second step;
  - the book-level title is English by Hardcover's policy, so searches match on the
    caller's wording and return what they actually matched.
"""

import json
import os
import urllib.error
import urllib.request

from mcp.server import MCPServer

API = "https://api.hardcover.app/v1/graphql"
TOKEN_PATH = os.environ.get("HARDCOVER_TOKEN_PATH", "hardcover.token")

STATUSES = {"want": 1, "reading": 2, "read": 3, "dnf": 5}

mcp = MCPServer("hardcover")


def _token() -> str:
    with open(TOKEN_PATH) as f:
        return f.read().strip()


def _gql(query: str, variables: dict | None = None):
    payload = {"query": query}
    if variables:
        payload["variables"] = variables
    req = urllib.request.Request(
        API,
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "User-Agent": "hardcover-mcp/0.1",
            "Authorization": "Bearer " + _token(),
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=45) as f:
            return json.loads(f.read().decode())
    except urllib.error.HTTPError as e:
        return {"error": e.code, "detail": e.read().decode()[:400]}
    except (urllib.error.URLError, ValueError) as e:
        return {"error": "request failed", "detail": str(e)[:400]}


def _search(query: str, per_page: int = 5) -> list[dict]:
    q = '{ search(query: %s, query_type: "Book", per_page: %d) { results } }' % (
        json.dumps(query),
        per_page,
    )
    res = _gql(q)
    try:
        hits = res["data"]["search"]["results"].get("hits", [])
    except (KeyError, TypeError):
        return []
    out = []
    for h in hits:
        d = h.get("document") or {}
        authors = d.get("author_names") or []
        book_id = d.get("id")
        if isinstance(book_id, str) and book_id.isdigit():
            book_id = int(book_id)
        out.append(
            {
                "book_id": book_id,
                "title": d.get("title"),
                "author": ", ".join(authors) if isinstance(authors, list) else authors,
                "year": d.get("release_year") or d.get("publication_year"),
                "alternative_titles": (d.get("alternative_titles") or [])[:5],
            }
        )
    return out


def _find_book(title: str, year: int | None = None) -> dict | None:
    """Best-effort match: exact title first, honour year when given.
    ponytail: fuzzy fallback returns the first candidate. If a wrong book ever gets
    written, pass a book_id from hardcover_search instead of a title."""
    candidates = _search(title, per_page=8)
    for c in candidates:
        if (c.get("title") or "").strip().lower() == title.strip().lower():
            if year is None or c.get("year") == year:
                return c
    if not candidates:
        return None
    if year is not None:
        for c in candidates:
            if c.get("year") == year:
                return c
    return candidates[0]


def _user_book(book_id: int) -> dict | None:
    q = "{ me { user_books(where: {book_id: {_eq: %d}}) { id status_id rating } } }" % book_id
    res = _gql(q)
    try:
        rows = res["data"]["me"][0]["user_books"]
    except (KeyError, TypeError, IndexError):
        return None
    return rows[0] if rows else None


def _set_finished_date(user_book_id: int, finished_at: str) -> str | None:
    """Create the read row, then set its date. Returns an error string on failure."""
    q = (
        "mutation { upsert_user_book_reads(user_book_id: %d, datesRead: [{finished_at: null}])"
        " { user_book_id } }" % user_book_id
    )
    _gql(q)
    q = "{ me { user_books(where: {id: {_eq: %d}}) { user_book_reads { id } } } }" % user_book_id
    res = _gql(q)
    try:
        reads = res["data"]["me"][0]["user_books"][0]["user_book_reads"]
    except (KeyError, TypeError, IndexError):
        return "could not read back the read record"
    if not reads:
        return "no read record was created"
    read_id = max(r["id"] for r in reads)
    q = (
        'mutation { update_user_book_read(id: %d, object: {finished_at: %s, finished_at_precision: 0})'
        " { id error } }" % (read_id, json.dumps(finished_at))
    )
    res = _gql(q)
    err = ((res.get("data") or {}).get("update_user_book_read") or {}).get("error")
    return None if not err else str(err)


def _ensure_user_book(title: str, year: int | None, status: str, stars: float | None):
    """Find or create the user_book. Returns (book, user_book_id, created, error)."""
    book = _find_book(title, year)
    if not book:
        return None, None, False, f"no book matched {title!r}"
    existing = _user_book(book["book_id"])
    if existing:
        return book, existing["id"], False, None
    fields = [f"book_id: {book['book_id']}", f"status_id: {STATUSES[status]}"]
    if stars is not None:
        fields.append(f"rating: {stars}")
    q = "mutation { insert_user_book(object: {%s}) { id error } }" % ", ".join(fields)
    res = _gql(q)
    node = (res.get("data") or {}).get("insert_user_book") or {}
    if node.get("error") or not node.get("id"):
        return book, None, False, str(node.get("error") or res)
    return book, node["id"], True, None


@mcp.tool()
def hardcover_search(query: str, per_page: int = 5) -> str:
    """Search Hardcover's catalogue by title or author. Unicode (e.g. Cyrillic) queries work.
    Returns candidates with book_id; use one before any write."""
    return json.dumps({"query": query, "results": _search(query, per_page)}, indent=1, ensure_ascii=False)


@mcp.tool()
def hardcover_library(status: str = "read", limit: int = 0) -> str:
    """The account's Hardcover library by status: 'read', 'reading', 'want' or 'dnf'.
    limit 0 returns everything. Includes rating and read dates."""
    if status not in STATUSES:
        return json.dumps({"error": "status must be one of " + ", ".join(STATUSES)})
    limit_clause = f", limit: {limit}" if limit else ""
    q = (
        "{ me { user_books(where: {status_id: {_eq: %d}}, order_by: {last_read_date: desc_nulls_last}%s) {"
        " id rating first_read_date last_read_date"
        " book { title contributions { author { name } } } } } }"
        % (STATUSES[status], limit_clause)
    )
    res = _gql(q)
    try:
        rows = res["data"]["me"][0]["user_books"]
    except (KeyError, TypeError, IndexError):
        return json.dumps({"error": "unexpected response", "detail": res})
    books = []
    for r in rows:
        authors = [
            c["author"]["name"]
            for c in (r["book"].get("contributions") or [])
            if c.get("author")
        ]
        books.append(
            {
                "user_book_id": r["id"],
                "title": r["book"]["title"],
                "author": ", ".join(authors),
                "rating": r.get("rating"),
                "finished_at": r.get("last_read_date"),
                "first_read": r.get("first_read_date"),
            }
        )
    return json.dumps({"status": status, "count": len(books), "books": books}, indent=1, ensure_ascii=False)


@mcp.tool()
def hardcover_mark_read(
    title: str, stars: float | None = None, finished_at: str | None = None, year: int | None = None
) -> str:
    """Mark a book as read, optionally with a rating (0.5-5) and finish date (YYYY-MM-DD).
    Handles the date in the follow-up call Hardcover needs. Confirms what it matched."""
    if stars is not None and not 0.5 <= stars <= 5:
        return json.dumps({"error": "stars must be between 0.5 and 5"})
    book, ub_id, created, err = _ensure_user_book(title, year, "read", stars)
    if err:
        return json.dumps({"error": err, "hint": "call hardcover_search"})
    fields = ["status_id: 3"]
    if stars is not None:
        fields.append(f"rating: {stars}")
    _gql("mutation { update_user_book(id: %d, object: {%s}) { id error } }" % (ub_id, ", ".join(fields)))
    date_error = _set_finished_date(ub_id, finished_at) if finished_at else None
    verified = _gql(
        "{ me { user_books(where: {id: {_eq: %d}}) { status_id rating last_read_date } } }" % ub_id
    )
    state = {}
    try:
        state = verified["data"]["me"][0]["user_books"][0]
    except (KeyError, TypeError, IndexError):
        pass
    return json.dumps(
        {
            "ok": state.get("status_id") == 3 and not date_error,
            "matched": f"{book['title']} ({book['author']})",
            "book_id": book["book_id"],
            "created": created,
            "final_state": state,
            "date_error": date_error,
        },
        indent=1,
        ensure_ascii=False,
    )


@mcp.tool()
def hardcover_set_rating(title: str, stars: float, year: int | None = None) -> str:
    """Set or change the rating (0.5-5) on a book already on a shelf."""
    if not 0.5 <= stars <= 5:
        return json.dumps({"error": "stars must be between 0.5 and 5"})
    book, ub_id, created, err = _ensure_user_book(title, year, "read", stars)
    if err:
        return json.dumps({"error": err, "hint": "call hardcover_search"})
    _gql("mutation { update_user_book(id: %d, object: {rating: %s}) { id error } }" % (ub_id, stars))
    verified = _gql("{ me { user_books(where: {id: {_eq: %d}}) { rating } } }" % ub_id)
    got = None
    try:
        got = verified["data"]["me"][0]["user_books"][0]["rating"]
    except (KeyError, TypeError, IndexError):
        pass
    return json.dumps(
        {"ok": got == stars, "matched": f"{book['title']} ({book['author']})", "rating": got, "created": created},
        indent=1,
        ensure_ascii=False,
    )


@mcp.tool()
def hardcover_set_status(title: str, status: str, year: int | None = None) -> str:
    """Move a book to a shelf: 'want', 'reading', 'read' or 'dnf'."""
    if status not in STATUSES:
        return json.dumps({"error": "status must be one of " + ", ".join(STATUSES)})
    book, ub_id, created, err = _ensure_user_book(title, year, status, None)
    if err:
        return json.dumps({"error": err, "hint": "call hardcover_search"})
    _gql(
        "mutation { update_user_book(id: %d, object: {status_id: %d}) { id error } }"
        % (ub_id, STATUSES[status])
    )
    verified = _gql("{ me { user_books(where: {id: {_eq: %d}}) { status_id } } }" % ub_id)
    got = None
    try:
        got = verified["data"]["me"][0]["user_books"][0]["status_id"]
    except (KeyError, TypeError, IndexError):
        pass
    return json.dumps(
        {
            "ok": got == STATUSES[status],
            "matched": f"{book['title']} ({book['author']})",
            "status": status,
            "created": created,
        },
        indent=1,
        ensure_ascii=False,
    )


@mcp.tool()
def hardcover_stats() -> str:
    """Library counts and the known data gap: how many read books still have no date."""
    res = _gql(
        "{ me { user_books { status_id rating first_read_date last_read_date } } }"
    )
    try:
        rows = res["data"]["me"][0]["user_books"]
    except (KeyError, TypeError, IndexError):
        return json.dumps({"error": "unexpected response", "detail": res})
    by_status: dict = {}
    for r in rows:
        by_status[r["status_id"]] = by_status.get(r["status_id"], 0) + 1
    read = [r for r in rows if r["status_id"] == 3]
    rated = [r["rating"] for r in read if r.get("rating")]
    undated = [r for r in read if not r.get("last_read_date") and not r.get("first_read_date")]
    return json.dumps(
        {
            "total": len(rows),
            "by_status_id": by_status,
            "read": len(read),
            "rated": len(rated),
            "average_rating": round(sum(rated) / len(rated), 2) if rated else None,
            "read_books_missing_dates": len(undated),
        },
        indent=1,
    )


if __name__ == "__main__":
    mcp.run()
