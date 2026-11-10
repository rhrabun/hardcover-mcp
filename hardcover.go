// Package main is an MCP server for Hardcover.
//
// Go note: every file in a directory declares its own `package` line; files with
// the same package name share everything, so there is no `import server` step like
// in Python. The package here is `main` because `go build` turns it into a binary,
// and `func main` at the bottom of main.go is the entry point.
//
// Three Hardcover quirks are handled here so callers never hit them:
//   - the finished date is silently dropped when a read record is first created, so it is
//     written in a second step and read back to confirm;
//   - inserting a book as read also stamps a read row dated Hardcover's own today, so a book
//     marked read without a date has that row deleted rather than keeping a date nobody gave;
//   - the book-level title is English by Hardcover's policy, so searches match on the
//     caller's wording and return what they actually matched.
package main

// Imports are always explicit. Each path is a package, and you refer to its
// exported names with the package name as prefix: json.Marshal, fmt.Sprintf,
// strings.Join. Python's `from x import y` / `import x` split does not exist;
// you get the whole package under its name (an alias or a dot-import is
// possible, but this code does not use them).
//
// gofmt sorts this block and keeps one blank line between the standard library
// and third-party packages. Running `gofmt -w .` (or `go fmt ./...`) is the only
// formatting command you need; arguing about layout in Go is not a thing.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// const declares compile-time constants. Grouped form is just style; each line
// is its own declaration. Unlike Python there is no module-level mutability
// check at runtime: Go's compiler rejects assigning to a const.
const (
	apiURL    = "https://api.hardcover.app/v1/graphql"
	userAgent = "hardcover-mcp/0.1"
)

// var declares package-level variables. Map literal syntax is
// `map[KeyType]ValueType{...}` and this is the rough equivalent of a Python dict
// for the string->int shelf ids Hardcover uses.
//
// Caveat that matters later: Go map iteration order is randomised on purpose,
// so never depend on it. When order is needed (tool error messages) the code
// below uses the separate statusNames slice instead.
var statusIDs = map[string]int{"want": 1, "reading": 2, "read": 3, "dnf": 5}

// statusNames exists only because a map cannot be iterated in a stable order.
// A slice literal is `[]Type{...}`: an array whose length can change.
var statusNames = []string{"want", "reading", "read", "dnf"}

// Client talks to Hardcover's GraphQL API.
//
// A struct is Go's closest thing to a class: a named collection of fields, with
// behaviour attached later through methods. There is no inheritance and no
// constructor keyword. Exported names (capitalised) are visible to other
// packages; unexported ones (lowercase, like readRow) are package-private, like
// Python's leading underscore but enforced by the compiler.
type Client struct {
	// TokenPath is the file holding the API token.
	TokenPath string

	// Do sends one GraphQL request and returns the raw response body.
	//
	// This is a function-typed field, and it is the Go answer to the Python
	// version's `server._gql = fake_gql` monkeypatch: tests construct a Client
	// and overwrite Do with a stub. Go has no runtime patching, so the seam has
	// to be designed in. `func(...) (...)` is a function type; `ctx` is passed
	// explicitly everywhere because Go has no implicit request context.
	Do func(ctx context.Context, query string, vars map[string]any) ([]byte, error)
}

// NewClient returns a ready Client. By convention Go has no __init__; a
// function named NewXxx plays the constructor role and returns a pointer.
// `*Client` means "pointer to Client", and the & operator below takes the
// address of the struct literal.
func NewClient() *Client {
	c := &Client{TokenPath: tokenPath()}
	// Method value: c.post is the method `post` bound to this Client, adapted
	// to the plain function type of the Do field. This is how the real
	// implementation gets injected by default while tests can swap it out.
	c.Do = c.post
	return c
}

// tokenPath resolves HARDCOVER_TOKEN_PATH, defaulting to ~/.config/hardcover/token.
//
// Python's os.path.expanduser is not replicated by os.Getenv; expanding `~`
// is a shell feature, and it has to be done by hand here.
func tokenPath() string {
	path := os.Getenv("HARDCOVER_TOKEN_PATH")
	// In Go, an unset environment variable and one set to the empty string are
	// both "". There is no `None` from os.environ.get.
	if path == "" {
		path = "~/.config/hardcover/token"
	}
	// strings.HasPrefix is Python's startswith. Note the explicit error check:
	// os.UserHomeDir returns (string, error), not just a string.
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			// If-scoped declaration: `home` and `err` exist only inside this
			// if/else chain. Very common Go idiom.
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// post performs the actual HTTP request. The receiver `(c *Client)` is how Go
// declares a method; think `self`, but typed and explicit.
//
// Multiple return values ([]byte, error) replace Python's exception habit.
// `error` is an ordinary interface value; nil means success. There is no
// try/except, so every call that can fail is followed by `if err != nil`.
func (c *Client) post(ctx context.Context, query string, vars map[string]any) ([]byte, error) {
	// The token is re-read on every call on purpose: a rotated token file takes
	// effect without restarting the MCP server.
	token, err := os.ReadFile(c.TokenPath)
	if err != nil {
		// %w wraps the original error so callers can inspect it with errors.Is
		// or errors.As. %v would print it and lose that link. Python's
		// `raise ... from err` is the same idea.
		return nil, fmt.Errorf("reading token: %w", err)
	}

	// map[string]any is the idiomatic Python `dict`. `any` is an alias for
	// interface{}, meaning "any value"; json.Marshal then decides the JSON
	// encoding by dynamic type.
	payload := map[string]any{"query": query}
	if len(vars) > 0 {
		payload["variables"] = vars
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// context.WithTimeout derives a child context that cancels itself after 45
	// seconds. `defer cancel()` schedules the cleanup for when post returns;
	// defers run LIFO. Skipping cancel leaks the timer until it fires, which
	// `go vet` will warn about. This replaces urllib's timeout parameter and
	// also cancels the request if the MCP client disconnects.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// bytes.NewReader wraps []byte in the io.Reader that http expects. The
	// short variable declaration `req, err :=` creates req and reuses err from
	// above; `:=` is required because req is new.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// string(token) converts []byte to string. strings.TrimSpace is Python's
	// .strip(); os.ReadFile does not trim the trailing newline of a token file.
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	// Closing the body is mandatory; this defer runs even on the early returns
	// below.
	defer resp.Body.Close()

	// io.ReadAll reads the whole body. Python's f.read() equivalent.
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	// Python's urllib raises HTTPError for non-2xx. net/http does not: the
	// response comes back normally and the status has to be checked by hand.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(out), 400))
	}
	return out, nil
}

// gql runs a query and decodes its data into out, or reports the GraphQL errors.
//
// `out any` is how Go spells "a pointer to whatever struct the caller wants
// filled in". json.Unmarshal needs a pointer because it mutates the value;
// passing a struct by value would silently decode into a copy.
func (c *Client) gql(ctx context.Context, query string, vars map[string]any, out any) error {
	raw, err := c.Do(ctx, query, vars)
	if err != nil {
		return err
	}

	// Anonymous struct: a type declared inline, used once, with no name. This
	// mirrors GraphQL's envelope {"data": ..., "errors": ...}.
	//
	// json.RawMessage is []byte: it keeps the nested JSON untouched instead of
	// decoding it. That is deliberate, because `data` has a different shape per
	// query and is decoded into the caller's type a few lines later.
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("unexpected response: %s", truncate(string(raw), 400))
	}
	// A missing key leaves RawMessage nil, hence both checks: len for absent,
	// "null" for explicitly null (GraphQL's usual empty-errors marker).
	if len(env.Errors) > 0 && string(env.Errors) != "null" {
		return fmt.Errorf("%s", truncate(string(env.Errors), 400))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("unexpected response: %s", truncate(string(raw), 400))
	}
	// `out == nil` compares the interface value to nil. Mutations that only
	// care about success versus error pass nil here.
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	return nil
}

// meRows decodes data.me[0].user_books into out.
//
// Every query in this server starts at `me`, so this helper removes the
// `res["data"]["me"][0]["user_books"]` indexing dance Python did by hand.
// Here the extraction is expressed as types instead of chained dict lookups.
func (c *Client) meRows(ctx context.Context, query string, out any) error {
	// `Me []struct{...}` is a slice of anonymous structs: GraphQL returns an
	// array here even though there is exactly one account. Slices are the
	// Python list equivalent and their zero value is nil, which len() treats
	// as zero.
	var env struct {
		Me []struct {
			UserBooks json.RawMessage `json:"user_books"`
		} `json:"me"`
	}
	if err := c.gql(ctx, query, nil, &env); err != nil {
		return err
	}
	// Bounds checks are explicit. A missing me[0], or a user_books that the
	// server answered with null, means the response cannot be trusted.
	if len(env.Me) == 0 || len(env.Me[0].UserBooks) == 0 {
		return fmt.Errorf("unexpected response: no account data")
	}
	// A RawMessage starts with '[' or '{' when present; an empty key is nil and
	// caught above. Decoding happens only now, into the caller's concrete type.
	if err := json.Unmarshal(env.Me[0].UserBooks, out); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	return nil
}

// truncate caps error text before it goes into a tool result. Byte slicing is
// safe here because the cut is only for readability; slicing may split a
// multi-byte rune, which is acceptable in an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// flexInt accepts a book id that Hardcover sends as either a string or a number.
//
// This is a defined type (`type X Y`, not an alias), so methods can be attached
// to it. Implementing UnmarshalJSON makes it a json.Unmarshaler: encoding/json
// notices the method and calls it instead of its own decoding. That is Go's
// duck typing, satisfied implicitly, like a Python protocol.
type flexInt int64

// UnmarshalJSON receives the raw JSON bytes. The pointer receiver is required
// because the method mutates *f; a value receiver would edit a copy.
func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	// `null` decodes as the zero value, matching the previous Python behaviour
	// of turning a missing id into 0.
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	// A leading quote means the id arrived as a JSON string; strip it and parse
	// the digits. strconv.ParseInt is int(s) but fallible, and it returns
	// multiple values (number, error) like most parsing helpers.
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	// Otherwise it is a JSON number. Decoding into int64 also rejects floats.
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// Book is one search hit.
//
// Struct tags (the text inside backticks) are metadata read by reflection.
// encoding/json uses `json:"..."` for field names; the MCP SDK uses
// `jsonschema:"..."` as the description shown to the model in the tool's output
// schema. Both are static strings, checked by the tooling rather than the
// compiler, so a typo shows up as a wrong key name in JSON.
//
// Every field is exported, because encoding/json only sees exported fields.
// *int is used for "known or unknown": a plain int cannot tell 0 from absent,
// while a nil pointer marshals to null. That is Go's replacement for None.
type Book struct {
	BookID            int64    `json:"book_id" jsonschema:"Hardcover book id; pass one from a search before any write"`
	Title             string   `json:"title" jsonschema:"book title as Hardcover stores it"`
	Author            string   `json:"author" jsonschema:"author names, comma separated"`
	Year              *int     `json:"year" jsonschema:"release or publication year, when known"`
	AlternativeTitles []string `json:"alternative_titles" jsonschema:"up to five alternative titles"`
}

// Search finds books by title or author. Unicode queries work.
//
// The return type ([]Book, error) is Go's "either a value or an error", with no
// exceptions to catch. Callers must check `err != nil` before trusting books.
func (c *Client) Search(ctx context.Context, query string, perPage int) ([]Book, error) {
	// Go has no default arguments. The Python signature's `per_page=5` becomes
	// an explicit zero check, and callers that want the default pass 0.
	if perPage <= 0 {
		perPage = 5
	}

	// The query text is a raw string literal (backticks): newlines are kept as
	// written and `\` is not an escape character. Only the user-supplied title
	// goes through GraphQL variables, never string interpolation, so quotes and
	// Cyrillic in a title cannot break out of the query.
	const q = `query ($q: String!, $n: Int!) {
  search(query: $q, query_type: "Book", per_page: $n) { results }
}`

	// Anonymous structs mirror the response shape. This is verbose, but it is
	// also the documentation and the validation: decoding into a struct with
	// tags names exactly the fields the code expects.
	var data struct {
		Search struct {
			Results struct {
				Hits []struct {
					Document struct {
						ID                flexInt  `json:"id"`
						Title             string   `json:"title"`
						AuthorNames       []string `json:"author_names"`
						ReleaseYear       *int     `json:"release_year"`
						PublicationYear   *int     `json:"publication_year"`
						AlternativeTitles []string `json:"alternative_titles"`
					} `json:"document"`
				} `json:"hits"`
			} `json:"results"`
		} `json:"search"`
	}
	// `&data` is required: json.Unmarshal fills the pointed-to value.
	if err := c.gql(ctx, q, map[string]any{"q": query, "n": perPage}, &data); err != nil {
		return nil, err
	}

	// make([]Book, 0, n) creates an empty, non-nil slice with room for n items
	// (capacity is a performance hint). A non-nil slice marshals to [] instead
	// of null, which keeps the tool output stable for agents reading it.
	books := make([]Book, 0, len(data.Search.Results.Hits))
	for _, hit := range data.Search.Results.Hits {
		// `_` is the blank identifier: "I do not need this value". Here it
		// discards the index that range yields alongside each element.
		doc := hit.Document
		year := doc.ReleaseYear
		if year == nil {
			year = doc.PublicationYear
		}
		alternatives := doc.AlternativeTitles
		if len(alternatives) > 5 {
			// Slice expressions are Python slicing without copying: [0:5] in
			// Python, [:5] here. The new slice shares the backing array.
			alternatives = alternatives[:5]
		}
		if alternatives == nil {
			// Normalise nil to empty so the JSON is [] rather than null.
			alternatives = []string{}
		}
		// append returns the (possibly reallocated) slice; always assign it
		// back. Forgetting the assignment is one of the classic Go bugs.
		books = append(books, Book{
			BookID:            int64(doc.ID),
			Title:             doc.Title,
			Author:            strings.Join(doc.AuthorNames, ", "),
			Year:              year,
			AlternativeTitles: alternatives,
		})
	}
	return books, nil
}

// findBook is a best-effort match: exact title first, honour year when given.
// A fuzzy fallback returns the first candidate; a wrong match is the caller's
// cue to pass a book_id from hardcover_search instead of a title.
//
// `year *int` is an optional argument: nil means "no year supplied", which is
// not expressible with a plain int. Returning (*Book, error) allows the
// "found nothing" case to be (nil, nil), keeping "absent" distinct from "failed"
// exactly as Python's None versus an exception did.
func (c *Client) findBook(ctx context.Context, title string, year *int) (*Book, error) {
	candidates, err := c.Search(ctx, title, 8)
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(title))
	// `for i := range candidates` iterates indices, not values. Indices are used
	// here because returning a pointer to a candidate must point into the
	// original slice; `&candidate` from a value loop would point at a copy.
	for i := range candidates {
		if strings.ToLower(strings.TrimSpace(candidates[i].Title)) != want {
			continue
		}
		// Deref syntax: *candidates[i].Year reads the int behind the pointer.
		// Both sides must be non-nil, hence the separate check; Go has no
		// Python-style `None or value` truthiness on pointers.
		if year == nil || (candidates[i].Year != nil && *candidates[i].Year == *year) {
			return &candidates[i], nil
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if year != nil {
		for i := range candidates {
			if candidates[i].Year != nil && *candidates[i].Year == *year {
				return &candidates[i], nil
			}
		}
	}
	// Indexing a slice is bounds-checked and panics on overflow, unlike Python
	// which raises IndexError; len() above guarantees this is safe.
	return &candidates[0], nil
}

// userBookID returns the user_book id for a book, or nil when it is not on a shelf.
//
// A nil *int64 plus a nil error means "looked successfully, found nothing". The
// pointer is essential: returning 0 would be indistinguishable from a real id.
func (c *Client) userBookID(ctx context.Context, bookID int64) (*int64, error) {
	// fmt.Sprintf is f-strings with printf verbs: %d int, %s string, %q quoted
	// string, %v default formatting. Interpolating an int64 this way is safe
	// because the type system guarantees it is a number, not arbitrary text.
	q := fmt.Sprintf("{ me { user_books(where: {book_id: {_eq: %d}}) { id } } }", bookID)
	// A slice of anonymous structs is a compact, local response type. There is
	// no need to name it because nothing else uses it.
	var rows []struct {
		ID int64 `json:"id"`
	}
	if err := c.meRows(ctx, q, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0].ID, nil
}

// readRow is one Hardcover read record: the thing that carries a finish date.
// Unexported, so it stays internal to this package, like a leading underscore.
type readRow struct {
	ID         int64   `json:"id"`
	FinishedAt *string `json:"finished_at"`
}

// State is one user_book's status, rating, dates and read-row count.
//
// Pointers throughout: nil means Hardcover reported no value, and it marshals
// to JSON null. ReadRows is a plain int because 0 is meaningful and correct.
type State struct {
	StatusID     *int      `json:"status_id" jsonschema:"shelf id: 1 want, 2 reading, 3 read, 5 did not finish"`
	Rating       *float64  `json:"rating" jsonschema:"rating from 0.5 to 5"`
	LastReadDate *string   `json:"last_read_date" jsonschema:"last read date as YYYY-MM-DD"`
	ReadRows     int       `json:"read_rows" jsonschema:"number of read records; zero means the book has no date"`
	ReadDates    []*string `json:"read_dates" jsonschema:"finish dates of the read records"`
}

// state reads a user_book. The row count is the fresh signal:
// last_read_date is derived from the read rows and can still show the old
// value in the response right after a row was deleted.
//
// State{} is the zero value of the struct (all fields nil/0). Go has no
// "empty dict" concept for structs; returning the zero value plus an error
// tells the caller nothing usable came back.
func (c *Client) state(ctx context.Context, userBookID int64) (State, error) {
	q := fmt.Sprintf(`{ me { user_books(where: {id: {_eq: %d}}) { status_id rating last_read_date
  user_book_reads { id finished_at } } } }`, userBookID)
	var rows []struct {
		StatusID      *int      `json:"status_id"`
		Rating        *float64  `json:"rating"`
		LastReadDate  *string   `json:"last_read_date"`
		UserBookReads []readRow `json:"user_book_reads"`
	}
	if err := c.meRows(ctx, q, &rows); err != nil {
		return State{}, err
	}
	if len(rows) == 0 {
		return State{}, nil
	}
	// row is a copy of rows[0]. That is fine: only its fields are read, and
	// nothing needs to mutate the slice.
	row := rows[0]
	// Pre-size the result slice to the number of read rows. Capacity only, so
	// appending below never reallocates in practice.
	dates := make([]*string, 0, len(row.UserBookReads))
	for _, read := range row.UserBookReads {
		dates = append(dates, read.FinishedAt)
	}
	return State{
		StatusID:     row.StatusID,
		Rating:       row.Rating,
		LastReadDate: row.LastReadDate,
		ReadRows:     len(row.UserBookReads),
		ReadDates:    dates,
	}, nil
}

// readRows fetches just the read records for a user_book.
func (c *Client) readRows(ctx context.Context, userBookID int64) ([]readRow, error) {
	q := fmt.Sprintf(`{ me { user_books(where: {id: {_eq: %d}}) { user_book_reads { id finished_at } } } }`, userBookID)
	var rows []struct {
		UserBookReads []readRow `json:"user_book_reads"`
	}
	if err := c.meRows(ctx, q, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].UserBookReads, nil
}

// clearAutoReads deletes the read row insert_user_book creates by itself.
// Inserting a book as read stamps a read row dated Hardcover's own today, a
// date the caller never gave. Removing it leaves the book read with no date,
// which is Hardcover's unknown-date state and the honest record.
//
// This method returns only error: there is no useful value for the caller, so
// nil means "done" and a non-nil error is the whole failure report.
func (c *Client) clearAutoReads(ctx context.Context, userBookID int64) error {
	rows, err := c.readRows(ctx, userBookID)
	if err != nil {
		return fmt.Errorf("could not read back the read rows to clear the auto-created date: %w", err)
	}
	for _, row := range rows {
		// One mutation per row, as the Python version did. The deleted id is
		// requested back as a success check.
		q := fmt.Sprintf("mutation { delete_user_book_read(id: %d) { id } }", row.ID)
		// Nested anonymous structs model `delete_user_book_read: {id: ...}`.
		// Both levels are pointers so a null payload is detectable rather than
		// silently becoming a zero struct.
		var out struct {
			Delete *struct {
				ID *int64 `json:"id"`
			} `json:"delete_user_book_read"`
		}
		// Note the shadowing: this err is scoped to the if statement and
		// shadows the outer err. Deliberate and common Go style; vet is happy
		// with it because the outer err is not used afterwards.
		if err := c.gql(ctx, q, nil, &out); err != nil {
			return fmt.Errorf("could not clear the auto-created date: %w", err)
		}
		if out.Delete == nil || out.Delete.ID == nil {
			return fmt.Errorf("could not clear the auto-created date: no id returned")
		}
	}
	return nil
}

// setReadDate puts the finish date on the book's read row, creating a row only
// when none exists. The date is confirmed by reading it back, because the write
// calls report success without carrying the value.
func (c *Client) setReadDate(ctx context.Context, userBookID int64, finishedAt string) error {
	rows, err := c.readRows(ctx, userBookID)
	if err != nil {
		return fmt.Errorf("could not read back the read record: %w", err)
	}
	// Declare q without a value; the zero value of string is "". Both branches
	// below must assign before use, which the compiler enforces.
	var q string
	if len(rows) > 0 {
		// Update the newest read row (largest id), not the first one returned.
		var maxID int64
		for _, row := range rows {
			if row.ID > maxID {
				maxID = row.ID
			}
		}
		// %q renders the date as a quoted, escaped Go string literal, which is
		// a valid GraphQL string literal for a validated YYYY-MM-DD value.
		q = fmt.Sprintf(`mutation { update_user_book_read(id: %d, object: {finished_at: %q, finished_at_precision: 0}) { id error } }`, maxID, finishedAt)
	} else {
		// insert_user_book_read takes a user_book_id plus the record fields.
		q = fmt.Sprintf(`mutation { insert_user_book_read(user_book_id: %d, user_book_read: {finished_at: %q, finished_at_precision: 0}) { id } }`, userBookID, finishedAt)
	}
	// nil output: the mutation's payload adds nothing the follow-up read does
	// not verify better.
	if err := c.gql(ctx, q, nil, nil); err != nil {
		return err
	}

	// Read the value back. This is the whole reason the write is not trusted.
	check := fmt.Sprintf("{ me { user_books(where: {id: {_eq: %d}}) { last_read_date } } }", userBookID)
	var got []struct {
		LastReadDate *string `json:"last_read_date"`
	}
	if err := c.meRows(ctx, check, &got); err != nil {
		return fmt.Errorf("could not confirm the date after writing it: %w", err)
	}
	if len(got) == 0 {
		return fmt.Errorf("could not confirm the date after writing it")
	}
	// Pointer checks first, deref second. Getting this order wrong is a panic,
	// not a NoneType error.
	if got[0].LastReadDate == nil || *got[0].LastReadDate != finishedAt {
		// A local, readable stand-in for Python's repr(None) in the message.
		shown := "<nil>"
		if got[0].LastReadDate != nil {
			shown = *got[0].LastReadDate
		}
		return fmt.Errorf("date did not land: the book shows %q", shown)
	}
	return nil
}

// ensureUserBook finds or creates the user_book.
//
// Four return values instead of a tuple/object. Named in the old Python
// docstring order: (book, user_book_id, created, error). Multi-value returns
// like this are why Go code often has `x, y, z, err := ...`.
func (c *Client) ensureUserBook(ctx context.Context, title string, year *int, status string, stars *float64) (*Book, int64, bool, error) {
	book, err := c.findBook(ctx, title, year)
	if err != nil {
		return nil, 0, false, fmt.Errorf("search failed: %w", err)
	}
	if book == nil {
		return nil, 0, false, fmt.Errorf("no book matched %q", title)
	}
	existing, err := c.userBookID(ctx, book.BookID)
	if err != nil {
		return nil, 0, false, fmt.Errorf("could not read the shelf: %w", err)
	}
	if existing != nil {
		return book, *existing, false, nil
	}

	// Build the mutation's object literal as text. Only ints and a validated
	// float go in, so there is nothing here that needs GraphQL variables.
	fields := fmt.Sprintf("book_id: %d, status_id: %d", book.BookID, statusIDs[status])
	if stars != nil {
		// %v on a float64 prints the shortest representation: 4.5 stays 4.5,
		// 5.0 prints as 5, which is a valid GraphQL numeric literal.
		fields += fmt.Sprintf(", rating: %v", *stars)
	}
	q := fmt.Sprintf("mutation { insert_user_book(object: {%s}) { id error } }", fields)
	var out struct {
		Insert *struct {
			ID    *int64  `json:"id"`
			Error *string `json:"error"`
		} `json:"insert_user_book"`
	}
	if err := c.gql(ctx, q, nil, &out); err != nil {
		return book, 0, false, err
	}
	// Hasura reports application failures inside `error` with a 200 status, so
	// three explicit checks: no payload, an error message, or no id.
	if out.Insert == nil {
		return book, 0, false, fmt.Errorf("insert_user_book failed: no record returned")
	}
	if out.Insert.Error != nil {
		return book, 0, false, fmt.Errorf("insert_user_book failed: %s", *out.Insert.Error)
	}
	if out.Insert.ID == nil {
		return book, 0, false, fmt.Errorf("insert_user_book failed: no id returned")
	}
	return book, *out.Insert.ID, true, nil
}

// updateUserBook applies an object literal to a user_book.
//
// object is a pre-built GraphQL fragment such as "status_id: 3" or
// "rating: 4.5". Keeping it a string lets every caller share one mutation
// shape; the values inside are numbers, never user text.
func (c *Client) updateUserBook(ctx context.Context, userBookID int64, object string) error {
	q := fmt.Sprintf("mutation { update_user_book(id: %d, object: {%s}) { id error } }", userBookID, object)
	var out struct {
		Update *struct {
			ID    *int64  `json:"id"`
			Error *string `json:"error"`
		} `json:"update_user_book"`
	}
	if err := c.gql(ctx, q, nil, &out); err != nil {
		return err
	}
	// Hasura can answer 200 with an error string; treat that as a failure.
	if out.Update != nil && out.Update.Error != nil {
		return fmt.Errorf("update_user_book failed: %s", *out.Update.Error)
	}
	return nil
}

// validateDate checks the caller's date before it reaches GraphQL.
//
// "2006-01-02" is not arbitrary: Go's time package uses this exact reference
// date (15:04:05 on 2 Jan 2006) as the layout language. It is the closest thing
// to Python's strftime, except the layout is written as a sample timestamp.
func validateDate(s string) error {
	if _, err := time.Parse("2006-01-02", s); err != nil {
		// The blank identifier discards the parsed time; only validity matters.
		return fmt.Errorf("finished_at must be a date as YYYY-MM-DD, got %q", s)
	}
	return nil
}
