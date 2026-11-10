// This file holds the MCP-facing layer: the tool input/output types and one
// handler method per tool. hardcover.go holds the GraphQL/HTTP layer.
package main

// The blank line below separates stdlib imports from third-party ones; gofmt
// keeps the grouping, the order inside a group is alphabetical.
import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	// The official MCP Go SDK. The module is github.com/modelcontextprotocol/go-sdk
	// and the package inside it is named mcp, so it is used as mcp.NewServer,
	// mcp.AddTool and so on. This is the only MCP dependency in the build.
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addTools registers every tool on the server.
//
// AddTool is a generic function: the concrete In and Out types are inferred
// from the handler passed as the last argument. That is why each handler below
// has the signature
//
//	func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)
//
// and no JSON schema is written by hand. The SDK reflects over In and Out,
// builds the schemas from the struct tags, validates arguments, unmarshals
// them, marshals Out into structuredContent and text content, and turns a
// non-nil error into an isError result. Returning nil for *mcp.CallToolResult
// means "let the SDK build it".
//
// The handlers are methods on *Client, and passing c.searchTool here is a
// method value: it captures c, so the tool keeps talking to the same client.
func addTools(s *mcp.Server, c *Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_search",
		// Description is what the model sees when choosing a tool. "Unicode
		// (e.g. Cyrillic) queries work" is a real user-facing hint, not a
		// decorative comment.
		Description: "Search Hardcover's catalogue by title or author. Unicode (e.g. Cyrillic) queries work. " +
			"Returns candidates with book_id; use one before any write.",
	}, c.searchTool)
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_library",
		Description: "The account's Hardcover library by status: 'read', 'reading', 'want' or 'dnf'. " +
			"limit 0 returns everything. Includes rating and read dates.",
	}, c.libraryTool)
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_mark_read",
		Description: "Mark a book as read, optionally with a rating (0.5-5) and finish date (YYYY-MM-DD). " +
			"Without a finish date the book is left read with no date rather than stamped with today. " +
			"Handles the two-step date write Hardcover needs and confirms what it matched.",
	}, c.markReadTool)
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_set_status",
		Description: "Move a book to a shelf: 'want', 'reading', 'read' or 'dnf'. Moving to 'read' without a date " +
			"leaves it read with no date; use hardcover_mark_read to record a finish date.",
	}, c.setStatusTool)
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_set_rating",
		Description: "Set or change the rating (0.5-5) on a book already on one of the shelves. " +
			"Use hardcover_mark_read or hardcover_set_status to put the book on a shelf first.",
	}, c.setRatingTool)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "hardcover_stats",
		Description: "Library counts and the known data gap: how many read books still have no date.",
	}, c.statsTool)
}

// SearchInput is the tool's argument schema.
//
// The `jsonschema:"..."` tag becomes the parameter description the model reads;
// the `json:"..."` name is what arrives on the wire. Start the name with a
// capital letter: unexported fields are ignored by encoding/json and by schema
// inference.
//
// Optional arguments are pointers with `omitempty`. There are no default values
// in the schema (and no Python-style `def f(x=5)`), so the handler applies the
// default. omitempty also keeps the key out of a marshalled request when nil.
type SearchInput struct {
	Query   string `json:"query" jsonschema:"title or author to search for; Unicode works"`
	PerPage *int   `json:"per_page,omitempty" jsonschema:"how many results to return (default 5)"`
}

// SearchOutput is the typed result. The SDK puts it into structuredContent and
// also emits its JSON as the text content, so old text-only clients keep
// working while newer ones can parse the structured copy.
type SearchOutput struct {
	Query   string `json:"query"`
	Results []Book `json:"results"`
}

// searchTool implements hardcover_search.
//
// `_ *mcp.CallToolRequest` discards the raw request: this handler needs none of
// it. The `_` keeps the signature the SDK requires without an unused-variable
// compile error (Go rejects unused variables, unlike unused parameters).
func (c *Client) searchTool(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	// Default first, override if the caller supplied a value. `perPage := 5`
	// is a short declaration; the type is inferred as int.
	perPage := 5
	if in.PerPage != nil {
		perPage = *in.PerPage
	}
	results, err := c.Search(ctx, in.Query, perPage)
	if err != nil {
		// Returning an error (third value) makes the SDK set isError on the
		// result and put the message in the text content. Nothing is thrown.
		return nil, SearchOutput{}, fmt.Errorf("search failed: %w", err)
	}
	// Composite literal with field names. Always name fields in multi-field
	// structs; positional literals break silently when a field is added.
	return nil, SearchOutput{Query: in.Query, Results: results}, nil
}

// LibraryInput is the argument schema for hardcover_library.
// Both fields are optional, so both are pointers.
type LibraryInput struct {
	Status *string `json:"status,omitempty" jsonschema:"shelf: read, reading, want or dnf (default read)"`
	Limit  *int    `json:"limit,omitempty" jsonschema:"how many books to return; 0 or omitted returns everything"`
}

// LibraryBook is one row of the library output.
type LibraryBook struct {
	UserBookID int64    `json:"user_book_id"`
	Title      string   `json:"title"`
	Author     string   `json:"author"`
	Rating     *float64 `json:"rating"`
	FinishedAt *string  `json:"finished_at"`
	FirstRead  *string  `json:"first_read"`
}

// LibraryOutput is the shelf listing result.
type LibraryOutput struct {
	Status string        `json:"status"`
	Count  int           `json:"count"`
	Books  []LibraryBook `json:"books"`
}

// libraryTool implements hardcover_library.
func (c *Client) libraryTool(ctx context.Context, _ *mcp.CallToolRequest, in LibraryInput) (*mcp.CallToolResult, LibraryOutput, error) {
	status := "read"
	if in.Status != nil {
		status = *in.Status
	}
	// The comma-ok idiom: a map lookup returns (value, found). Without the
	// second value, a missing key would return the zero value and look valid.
	statusID, ok := statusIDs[status]
	if !ok {
		return nil, LibraryOutput{}, fmt.Errorf("status must be one of %s", strings.Join(statusNames, ", "))
	}
	// The limit clause is built separately because "%s" with "" yields no
	// trailing comma issue. Only an int is interpolated, so this is safe.
	limitClause := ""
	if in.Limit != nil && *in.Limit != 0 {
		limitClause = fmt.Sprintf(", limit: %d", *in.Limit)
	}
	q := fmt.Sprintf(`{ me { user_books(where: {status_id: {_eq: %d}}, order_by: {last_read_date: desc_nulls_last}%s) {
  id rating first_read_date last_read_date
  book { title contributions { author { name } } } } } }`, statusID, limitClause)

	// Deeply nested anonymous structs. This is the price of typed decoding:
	// every level of the JSON response has to be named. Keeping it local means
	// there is no exported type for something only this handler sees.
	var rows []struct {
		ID            int64    `json:"id"`
		Rating        *float64 `json:"rating"`
		FirstReadDate *string  `json:"first_read_date"`
		LastReadDate  *string  `json:"last_read_date"`
		Book          struct {
			Title         string `json:"title"`
			Contributions []struct {
				Author *struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"contributions"`
		} `json:"book"`
	}
	if err := c.meRows(ctx, q, &rows); err != nil {
		return nil, LibraryOutput{}, err
	}

	// Pre-allocate with the exact length; append below still has to be
	// reassigned, which the compiler checks.
	books := make([]LibraryBook, 0, len(rows))
	for _, row := range rows {
		// Contributions can include non-author roles, so author is a pointer
		// and nil entries are skipped, matching the Python code.
		authors := make([]string, 0, len(row.Book.Contributions))
		for _, contribution := range row.Book.Contributions {
			if contribution.Author != nil {
				authors = append(authors, contribution.Author.Name)
			}
		}
		books = append(books, LibraryBook{
			UserBookID: row.ID,
			Title:      row.Book.Title,
			Author:     strings.Join(authors, ", "),
			Rating:     row.Rating,
			FinishedAt: row.LastReadDate,
			FirstRead:  row.FirstReadDate,
		})
	}
	return nil, LibraryOutput{Status: status, Count: len(books), Books: books}, nil
}

// MarkReadInput is the argument schema for hardcover_mark_read.
// Stars and FinishedAt are optional; Title is required, so it is a plain string.
type MarkReadInput struct {
	Title      string   `json:"title" jsonschema:"book title; call hardcover_search first if unsure"`
	Stars      *float64 `json:"stars,omitempty" jsonschema:"rating from 0.5 to 5"`
	FinishedAt *string  `json:"finished_at,omitempty" jsonschema:"finish date as YYYY-MM-DD; omit to leave the book read with no date"`
	Year       *int     `json:"year,omitempty" jsonschema:"publication year, to disambiguate a title"`
}

// MarkReadOutput reports what actually landed in Hardcover.
// OK is the post-write verification: everything below it exists so the caller
// can see why OK is false. DateError null means no date problem.
type MarkReadOutput struct {
	OK         bool    `json:"ok"`
	Matched    string  `json:"matched"`
	BookID     int64   `json:"book_id"`
	Created    bool    `json:"created"`
	FinalState State   `json:"final_state"`
	DateError  *string `json:"date_error"`
}

// markReadTool implements hardcover_mark_read.
func (c *Client) markReadTool(ctx context.Context, _ *mcp.CallToolRequest, in MarkReadInput) (*mcp.CallToolResult, MarkReadOutput, error) {
	// Validation happens before any network call. Returning an error here is a
	// tool error (isError), which is what an agent sees as a failed call.
	if in.Stars != nil && (*in.Stars < 0.5 || *in.Stars > 5) {
		return nil, MarkReadOutput{}, fmt.Errorf("stars must be between 0.5 and 5")
	}
	if in.FinishedAt != nil {
		if err := validateDate(*in.FinishedAt); err != nil {
			return nil, MarkReadOutput{}, err
		}
	}

	// Four-value return: find or create the shelf entry. `created` tells us
	// whether Hardcover stamped its own read date, which needs cleaning up.
	book, userBookID, created, err := c.ensureUserBook(ctx, in.Title, in.Year, "read", in.Stars)
	if err != nil {
		// %w keeps the original error connected so callers can unwrap it; the
		// hint is appended because an agent reads the text and nothing else.
		return nil, MarkReadOutput{}, fmt.Errorf("%w; call hardcover_search", err)
	}

	// Build the update object. Only numbers end up in it.
	object := "status_id: 3"
	if in.Stars != nil {
		object += fmt.Sprintf(", rating: %v", *in.Stars)
	}
	if err := c.updateUserBook(ctx, userBookID, object); err != nil {
		return nil, MarkReadOutput{}, err
	}

	// A failure here is not returned as a tool error: it is reported in
	// date_error with ok=false, because the status write may have succeeded.
	// `var dateErr error` is a nil error by default.
	var dateErr error
	if in.FinishedAt != nil {
		dateErr = c.setReadDate(ctx, userBookID, *in.FinishedAt)
	} else if created {
		dateErr = c.clearAutoReads(ctx, userBookID)
	}

	// Read the final state back; this is what makes OK trustworthy.
	state, err := c.state(ctx, userBookID)
	if err != nil {
		return nil, MarkReadOutput{}, err
	}
	// Short-circuit AND: Go's && stops at the first false, so the deref is
	// only reached when StatusID is non-nil. Order matters here.
	ok := state.StatusID != nil && *state.StatusID == 3 && dateErr == nil
	if in.FinishedAt != nil {
		ok = ok && containsDate(state.ReadDates, *in.FinishedAt)
	} else if created {
		// No date was asked for, so the book must end up with no read row.
		ok = ok && state.ReadRows == 0
	}

	out := MarkReadOutput{
		OK:         ok,
		Matched:    fmt.Sprintf("%s (%s)", book.Title, book.Author),
		BookID:     book.BookID,
		Created:    created,
		FinalState: state,
	}
	if dateErr != nil {
		// Making a pointer to a local is safe in Go: escape analysis moves the
		// value to the heap when its address outlives the function.
		message := dateErr.Error()
		out.DateError = &message
	}
	return nil, out, nil
}

// SetStatusInput is the argument schema for hardcover_set_status.
type SetStatusInput struct {
	Title  string `json:"title" jsonschema:"book title; call hardcover_search first if unsure"`
	Status string `json:"status" jsonschema:"shelf: want, reading, read or dnf"`
	Year   *int   `json:"year,omitempty" jsonschema:"publication year, to disambiguate a title"`
}

// SetStatusOutput reports the shelf move and the verified final state.
type SetStatusOutput struct {
	OK         bool    `json:"ok"`
	Matched    string  `json:"matched"`
	Status     string  `json:"status"`
	Created    bool    `json:"created"`
	FinalState State   `json:"final_state"`
	DateError  *string `json:"date_error"`
}

// setStatusTool implements hardcover_set_status.
func (c *Client) setStatusTool(ctx context.Context, _ *mcp.CallToolRequest, in SetStatusInput) (*mcp.CallToolResult, SetStatusOutput, error) {
	statusID, ok := statusIDs[in.Status]
	if !ok {
		return nil, SetStatusOutput{}, fmt.Errorf("status must be one of %s", strings.Join(statusNames, ", "))
	}
	// nil for stars: moving shelves does not touch the rating.
	book, userBookID, created, err := c.ensureUserBook(ctx, in.Title, in.Year, in.Status, nil)
	if err != nil {
		return nil, SetStatusOutput{}, fmt.Errorf("%w; call hardcover_search", err)
	}
	if err := c.updateUserBook(ctx, userBookID, fmt.Sprintf("status_id: %d", statusID)); err != nil {
		return nil, SetStatusOutput{}, err
	}
	// Only a freshly created "read" has the auto-stamped date row worth
	// deleting; an existing shelf entry keeps whatever history it had.
	var dateErr error
	if created && in.Status == "read" {
		dateErr = c.clearAutoReads(ctx, userBookID)
	}
	state, err := c.state(ctx, userBookID)
	if err != nil {
		return nil, SetStatusOutput{}, err
	}
	// `ok` is reassigned, not redeclared: it already exists from the map lookup.
	ok = state.StatusID != nil && *state.StatusID == statusID && dateErr == nil
	if created && in.Status == "read" {
		ok = ok && state.ReadRows == 0
	}
	out := SetStatusOutput{
		OK:         ok,
		Matched:    fmt.Sprintf("%s (%s)", book.Title, book.Author),
		Status:     in.Status,
		Created:    created,
		FinalState: state,
	}
	if dateErr != nil {
		message := dateErr.Error()
		out.DateError = &message
	}
	return nil, out, nil
}

// SetRatingInput is the argument schema for hardcover_set_rating.
// Stars is required here, so it is a plain float64 rather than a pointer.
type SetRatingInput struct {
	Title string  `json:"title" jsonschema:"book title; call hardcover_search first if unsure"`
	Stars float64 `json:"stars" jsonschema:"rating from 0.5 to 5"`
	Year  *int    `json:"year,omitempty" jsonschema:"publication year, to disambiguate a title"`
}

// SetRatingOutput echoes the rating read back from Hardcover.
type SetRatingOutput struct {
	OK      bool     `json:"ok"`
	Matched string   `json:"matched"`
	Rating  *float64 `json:"rating"`
}

// setRatingTool implements hardcover_set_rating.
func (c *Client) setRatingTool(ctx context.Context, _ *mcp.CallToolRequest, in SetRatingInput) (*mcp.CallToolResult, SetRatingOutput, error) {
	if in.Stars < 0.5 || in.Stars > 5 {
		return nil, SetRatingOutput{}, fmt.Errorf("stars must be between 0.5 and 5")
	}
	book, err := c.findBook(ctx, in.Title, in.Year)
	if err != nil {
		return nil, SetRatingOutput{}, fmt.Errorf("search failed: %w", err)
	}
	if book == nil {
		return nil, SetRatingOutput{}, fmt.Errorf("no book matched %q; call hardcover_search", in.Title)
	}
	userBookID, err := c.userBookID(ctx, book.BookID)
	if err != nil {
		return nil, SetRatingOutput{}, fmt.Errorf("could not read the shelf: %w", err)
	}
	// Refuse to create the shelf entry here: rating a book that is not on a
	// shelf would silently add it to the library.
	if userBookID == nil {
		return nil, SetRatingOutput{}, fmt.Errorf("%q is not on a shelf; call hardcover_mark_read or hardcover_set_status first", book.Title)
	}
	// Deref inside the call: updateUserBook takes the id by value.
	if err := c.updateUserBook(ctx, *userBookID, fmt.Sprintf("rating: %v", in.Stars)); err != nil {
		return nil, SetRatingOutput{}, err
	}
	state, err := c.state(ctx, *userBookID)
	if err != nil {
		return nil, SetRatingOutput{}, err
	}
	// Floating-point equality is fine here: both sides went through the same
	// JSON encoding of a half-step value, not accumulated arithmetic.
	ok := state.Rating != nil && *state.Rating == in.Stars
	return nil, SetRatingOutput{
		OK:      ok,
		Matched: fmt.Sprintf("%s (%s)", book.Title, book.Author),
		Rating:  state.Rating,
	}, nil
}

// StatsInput is empty: hardcover_stats takes no arguments.
//
// `struct{}` is the empty struct. It has no fields and no size, and it is the
// idiomatic "no parameters" type for a generic API like AddTool. The SDK infers
// an object schema with no properties.
type StatsInput struct{}

// StatsOutput is the count summary.
//
// ByStatusID is map[string]int, not map[int]int: JSON object keys are strings,
// and the SDK's schema inference only supports string-keyed maps. The int keys
// of the Python dict become decimal strings here.
type StatsOutput struct {
	Total                 int            `json:"total"`
	ByStatusID            map[string]int `json:"by_status_id" jsonschema:"user_book counts keyed by status id"`
	Read                  int            `json:"read"`
	Rated                 int            `json:"rated"`
	AverageRating         *float64       `json:"average_rating"`
	ReadBooksMissingDates int            `json:"read_books_missing_dates" jsonschema:"read books with neither a first nor a last read date"`
}

// statsTool implements hardcover_stats.
//
// The second `_` discards the empty input, since there is nothing to read.
func (c *Client) statsTool(ctx context.Context, _ *mcp.CallToolRequest, _ StatsInput) (*mcp.CallToolResult, StatsOutput, error) {
	const q = `{ me { user_books { status_id rating first_read_date last_read_date } } }`
	var rows []struct {
		StatusID      int      `json:"status_id"`
		Rating        *float64 `json:"rating"`
		FirstReadDate *string  `json:"first_read_date"`
		LastReadDate  *string  `json:"last_read_date"`
	}
	if err := c.meRows(ctx, q, &rows); err != nil {
		return nil, StatsOutput{}, err
	}

	byStatus := make(map[string]int, len(rows))
	// Grouping declaration: three ints and a float64, all zero-valued.
	var read, rated, missing int
	var sum float64
	for _, row := range rows {
		// strconv.Itoa is int-to-string. map[key]++ is a valid statement and
		// reads the zero value 0 for a new key, so no explicit initialisation
		// is needed.
		byStatus[strconv.Itoa(row.StatusID)]++
		// continue skips to the next element, like Python's continue.
		if row.StatusID != 3 {
			continue
		}
		read++
		if row.Rating != nil {
			rated++
			sum += *row.Rating
		}
		if row.LastReadDate == nil && row.FirstReadDate == nil {
			missing++
		}
	}
	out := StatsOutput{
		Total:                 len(rows),
		ByStatusID:            byStatus,
		Read:                  read,
		Rated:                 rated,
		ReadBooksMissingDates: missing,
	}
	if rated > 0 {
		// Average is nil (JSON null) when nothing is rated, matching the
		// Python None. float64(rated) is an explicit numeric conversion; Go
		// never converts between numeric types implicitly.
		average := math.Round(sum/float64(rated)*100) / 100
		out.AverageRating = &average
	}
	return nil, out, nil
}

// containsDate searches the read dates for a given one.
//
// Ranges over values work here because nothing needs the index and nothing
// takes an address. A nil entry is skipped before dereferencing.
func containsDate(dates []*string, want string) bool {
	for _, date := range dates {
		if date != nil && *date == want {
			return true
		}
	}
	return false
}
