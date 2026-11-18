// Package main is an MCP server for Hardcover.
//
// Three Hardcover quirks are handled here so callers never hit them:
//   - the finished date is silently dropped when a read record is first created, so it is
//     written in a second step and read back to confirm;
//   - inserting a book as read also stamps a read row dated Hardcover's own today, so a book
//     marked read without a date has that row deleted rather than keeping a date nobody gave;
//   - the book-level title is English by Hardcover's policy, so searches match on the
//     caller's wording and return what they actually matched.
package main

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

const (
	apiURL    = "https://api.hardcover.app/v1/graphql"
	userAgent = "hardcover-mcp/0.1"

	// Hardcover's read replica can lag a fresh insert; one retry covers it.
	defaultReadBackDelay = 1500 * time.Millisecond
)

var statusIDs = map[string]int{
	"want": 1, "reading": 2, "read": 3, "paused": 4, "dnf": 5, "ignored": 6,
}

var statusNames = []string{"want", "reading", "read", "paused", "dnf", "ignored"}

// Client talks to Hardcover's GraphQL API.
type Client struct {
	// TokenPath is the file holding the API token.
	TokenPath string
	// Do sends one GraphQL request and returns the raw response body.
	// Tests replace it.
	Do func(ctx context.Context, query string, vars map[string]any) ([]byte, error)

	// readBackDelay is how long to wait before retrying a verification read.
	readBackDelay time.Duration
}

func NewClient() (*Client, error) {
	path, err := tokenPath()
	if err != nil {
		return nil, err
	}
	c := &Client{TokenPath: path, readBackDelay: defaultReadBackDelay}
	c.Do = c.post
	return c, nil
}

func tokenPath() (string, error) {
	path := os.Getenv("HARDCOVER_TOKEN_PATH")
	if path == "" {
		path = "~/.config/hardcover/token"
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding home directory: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return path, nil
}

func (c *Client) post(ctx context.Context, query string, vars map[string]any) ([]byte, error) {
	token, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading token: %w", err)
	}
	payload := map[string]any{"query": query}
	if len(vars) > 0 {
		payload["variables"] = vars
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(out), 400))
	}
	return out, nil
}

// gql runs a query and decodes its data into out, or reports the GraphQL errors.
func (c *Client) gql(ctx context.Context, query string, vars map[string]any, out any) error {
	raw, err := c.Do(ctx, query, vars)
	if err != nil {
		return err
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("unexpected response: %s", truncate(string(raw), 400))
	}
	if len(env.Errors) > 0 && string(env.Errors) != "null" {
		return fmt.Errorf("%s", truncate(string(env.Errors), 400))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("unexpected response: %s", truncate(string(raw), 400))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	return nil
}

// meRows decodes data.me[0].user_books into out.
func (c *Client) meRows(ctx context.Context, query string, out any) error {
	var env struct {
		Me []struct {
			UserBooks json.RawMessage `json:"user_books"`
		} `json:"me"`
	}
	if err := c.gql(ctx, query, nil, &env); err != nil {
		return err
	}
	if len(env.Me) == 0 {
		return fmt.Errorf("unexpected response: no account data")
	}
	books := bytes.TrimSpace(env.Me[0].UserBooks)
	if len(books) == 0 || string(books) == "null" {
		return nil
	}
	if err := json.Unmarshal(books, out); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	return nil
}

// userBookRaw reads one user_book row, retrying once. Hardcover's read replica
// can lag a fresh insert, and an empty answer then looked like a failed write.
// A nil row with a nil error means it still could not be read.
func (c *Client) userBookRaw(ctx context.Context, userBookID int64, fields string) (json.RawMessage, error) {
	return c.userBookRawWhen(ctx, userBookID, fields, nil)
}

// userBookRawWhen is userBookRaw with a usability test. The replica can also
// serve a row whose derived value has not caught up yet, so a caller that
// writes a field then reads it back needs the retry to fire on a stale value,
// not only on a missing row. accept returns false to retry. When neither
// attempt is accepted, the last row read comes back with a nil error and the
// caller decides what to report.
func (c *Client) userBookRawWhen(ctx context.Context, userBookID int64, fields string, accept func(json.RawMessage) bool) (json.RawMessage, error) {
	q := fmt.Sprintf("{ me { user_books(where: {id: {_eq: %d}}) { %s } } }", userBookID, fields)
	var last json.RawMessage
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.readBackDelay):
			}
		}
		var env struct {
			Me []struct {
				UserBooks json.RawMessage `json:"user_books"`
			} `json:"me"`
		}
		if err := c.gql(ctx, q, nil, &env); err != nil {
			return nil, err
		}
		if len(env.Me) == 0 {
			return nil, fmt.Errorf("unexpected response: no account data")
		}
		var rows []json.RawMessage
		books := bytes.TrimSpace(env.Me[0].UserBooks)
		if len(books) > 0 && string(books) != "null" {
			if err := json.Unmarshal(books, &rows); err != nil {
				return nil, fmt.Errorf("unexpected response: %w", err)
			}
		}
		if len(rows) > 0 {
			last = rows[0]
			if accept == nil || accept(rows[0]) {
				return rows[0], nil
			}
		}
	}
	return last, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// flexInt accepts a book id that Hardcover sends as either a string or a number.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
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
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// Book is one search hit.
type Book struct {
	BookID            int64    `json:"book_id" jsonschema:"Hardcover book id; pass one from a search before any write"`
	Title             string   `json:"title" jsonschema:"book title as Hardcover stores it"`
	Author            string   `json:"author" jsonschema:"author names, comma separated"`
	Year              *int     `json:"year" jsonschema:"release or publication year, when known"`
	AlternativeTitles []string `json:"alternative_titles" jsonschema:"up to five alternative titles"`
}

// Search finds books by title or author. Unicode queries work.
func (c *Client) Search(ctx context.Context, query string, perPage int) ([]Book, error) {
	if perPage <= 0 {
		perPage = 5
	}
	const q = `query ($q: String!, $n: Int!) {
  search(query: $q, query_type: "Book", per_page: $n) { results }
}`
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
	if err := c.gql(ctx, q, map[string]any{"q": query, "n": perPage}, &data); err != nil {
		return nil, err
	}
	books := make([]Book, 0, len(data.Search.Results.Hits))
	for _, hit := range data.Search.Results.Hits {
		doc := hit.Document
		year := doc.ReleaseYear
		if year == nil {
			year = doc.PublicationYear
		}
		alternatives := doc.AlternativeTitles
		if len(alternatives) > 5 {
			alternatives = alternatives[:5]
		}
		if alternatives == nil {
			alternatives = []string{}
		}
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
func (c *Client) findBook(ctx context.Context, title string, year *int) (*Book, error) {
	candidates, err := c.Search(ctx, title, 8)
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(title))
	for i := range candidates {
		if strings.ToLower(strings.TrimSpace(candidates[i].Title)) != want {
			continue
		}
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
	return &candidates[0], nil
}

// userBookID returns the user_book id for a book, or nil when it is not on a shelf.
func (c *Client) userBookID(ctx context.Context, bookID int64) (*int64, error) {
	q := fmt.Sprintf("{ me { user_books(where: {book_id: {_eq: %d}}) { id } } }", bookID)
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

type readRow struct {
	ID         int64   `json:"id"`
	FinishedAt *string `json:"finished_at"`
}

// State is one user_book's status, rating, dates and read-row count.
type State struct {
	StatusID     *int      `json:"status_id" jsonschema:"shelf id: 1 want, 2 reading, 3 read, 4 paused, 5 did not finish, 6 ignored"`
	Rating       *float64  `json:"rating" jsonschema:"rating from 0.5 to 5"`
	LastReadDate *string   `json:"last_read_date" jsonschema:"last read date as YYYY-MM-DD"`
	ReadRows     int       `json:"read_rows" jsonschema:"number of read records; zero means the book has no date"`
	ReadDates    []*string `json:"read_dates" jsonschema:"finish dates of the read records"`
	ReadError    *string   `json:"read_error,omitempty" jsonschema:"set when the book could not be read back after a write"`
}

// state reads a user_book. The row count is the fresh signal:
// last_read_date is derived from the read rows and can still show the old
// value in the response right after a row was deleted.
func (c *Client) state(ctx context.Context, userBookID int64) (State, error) {
	raw, err := c.userBookRaw(ctx, userBookID, "status_id rating last_read_date user_book_reads { id finished_at }")
	if err != nil {
		return State{}, err
	}
	if raw == nil {
		message := "could not read the book back"
		return State{ReadError: &message}, nil
	}
	var row struct {
		StatusID      *int      `json:"status_id"`
		Rating        *float64  `json:"rating"`
		LastReadDate  *string   `json:"last_read_date"`
		UserBookReads []readRow `json:"user_book_reads"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return State{}, fmt.Errorf("unexpected response: %w", err)
	}
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

func (c *Client) readRows(ctx context.Context, userBookID int64) ([]readRow, error) {
	raw, err := c.userBookRaw(ctx, userBookID, "user_book_reads { id finished_at }")
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("could not read the book back")
	}
	var row struct {
		UserBookReads []readRow `json:"user_book_reads"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	return row.UserBookReads, nil
}

// clearAutoReads deletes the read row insert_user_book creates by itself.
// Inserting a book as read stamps a read row dated Hardcover's own today, a
// date the caller never gave. Removing it leaves the book read with no date,
// which is Hardcover's unknown-date state and the honest record.
func (c *Client) clearAutoReads(ctx context.Context, userBookID int64) error {
	rows, err := c.readRows(ctx, userBookID)
	if err != nil {
		return fmt.Errorf("could not read back the read rows to clear the auto-created date: %w", err)
	}
	for _, row := range rows {
		q := fmt.Sprintf("mutation { delete_user_book_read(id: %d) { id } }", row.ID)
		var out struct {
			Delete *struct {
				ID *int64 `json:"id"`
			} `json:"delete_user_book_read"`
		}
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
	var q string
	if len(rows) > 0 {
		var maxID int64
		for _, row := range rows {
			if row.ID > maxID {
				maxID = row.ID
			}
		}
		q = fmt.Sprintf(`mutation { update_user_book_read(id: %d, object: {finished_at: %q, finished_at_precision: 0}) { id error } }`, maxID, finishedAt)
	} else {
		q = fmt.Sprintf(`mutation { insert_user_book_read(user_book_id: %d, user_book_read: {finished_at: %q, finished_at_precision: 0}) { id } }`, userBookID, finishedAt)
	}
	if err := c.gql(ctx, q, nil, nil); err != nil {
		return err
	}
	raw, err := c.userBookRawWhen(ctx, userBookID, "last_read_date", func(candidate json.RawMessage) bool {
		var row struct {
			LastReadDate *string `json:"last_read_date"`
		}
		if err := json.Unmarshal(candidate, &row); err != nil {
			return false
		}
		return row.LastReadDate != nil && *row.LastReadDate == finishedAt
	})
	if err != nil {
		return fmt.Errorf("could not confirm the date after writing it: %w", err)
	}
	if raw == nil {
		return fmt.Errorf("could not confirm the date after writing it")
	}
	var row struct {
		LastReadDate *string `json:"last_read_date"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if row.LastReadDate == nil || *row.LastReadDate != finishedAt {
		shown := "<nil>"
		if row.LastReadDate != nil {
			shown = *row.LastReadDate
		}
		return fmt.Errorf("date did not land: the book shows %q", shown)
	}
	return nil
}

// ensureUserBook finds or creates the user_book.
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
	fields := fmt.Sprintf("book_id: %d, status_id: %d", book.BookID, statusIDs[status])
	if stars != nil {
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
	if out.Update != nil && out.Update.Error != nil {
		return fmt.Errorf("update_user_book failed: %s", *out.Update.Error)
	}
	return nil
}

func validateDate(s string) error {
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return fmt.Errorf("finished_at must be a date as YYYY-MM-DD, got %q", s)
	}
	return nil
}
