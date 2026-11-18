package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addTools(s *mcp.Server, c *Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_search",
		Description: "Search Hardcover's catalogue by title or author. Unicode (e.g. Cyrillic) queries work. " +
			"Returns candidates with book_id; use one before any write.",
	}, c.searchTool)
	mcp.AddTool(s, &mcp.Tool{
		Name: "hardcover_library",
		Description: "The account's Hardcover library by status: 'read', 'reading', 'want', 'paused', 'dnf' or 'ignored'. " +
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
		Description: "Move a book to a shelf: 'want', 'reading', 'read', 'paused', 'dnf' or 'ignored'. Moving to 'read' without a date " +
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

type SearchInput struct {
	Query   string `json:"query" jsonschema:"title or author to search for; Unicode works"`
	PerPage *int   `json:"per_page,omitempty" jsonschema:"how many results to return (default 5)"`
}

type SearchOutput struct {
	Query   string `json:"query"`
	Results []Book `json:"results"`
}

func (c *Client) searchTool(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	perPage := 5
	if in.PerPage != nil {
		perPage = *in.PerPage
	}
	results, err := c.Search(ctx, in.Query, perPage)
	if err != nil {
		return nil, SearchOutput{}, fmt.Errorf("search failed: %w", err)
	}
	return nil, SearchOutput{Query: in.Query, Results: results}, nil
}

type LibraryInput struct {
	Status *string `json:"status,omitempty" jsonschema:"shelf: read, reading, want, paused, dnf or ignored (default read)"`
	Limit  *int    `json:"limit,omitempty" jsonschema:"how many books to return; 0 or omitted returns everything"`
}

type LibraryBook struct {
	UserBookID int64    `json:"user_book_id"`
	Title      string   `json:"title"`
	Author     string   `json:"author"`
	Rating     *float64 `json:"rating"`
	FinishedAt *string  `json:"finished_at"`
	FirstRead  *string  `json:"first_read"`
}

type LibraryOutput struct {
	Status string        `json:"status"`
	Count  int           `json:"count"`
	Books  []LibraryBook `json:"books"`
}

func (c *Client) libraryTool(ctx context.Context, _ *mcp.CallToolRequest, in LibraryInput) (*mcp.CallToolResult, LibraryOutput, error) {
	status := "read"
	if in.Status != nil {
		status = *in.Status
	}
	statusID, ok := statusIDs[status]
	if !ok {
		return nil, LibraryOutput{}, fmt.Errorf("status must be one of %s", strings.Join(statusNames, ", "))
	}
	limitClause := ""
	if in.Limit != nil && *in.Limit != 0 {
		limitClause = fmt.Sprintf(", limit: %d", *in.Limit)
	}
	q := fmt.Sprintf(`{ me { user_books(where: {status_id: {_eq: %d}}, order_by: {last_read_date: desc_nulls_last}%s) {
  id rating first_read_date last_read_date
  book { title contributions { author { name } } } } } }`, statusID, limitClause)
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
	books := make([]LibraryBook, 0, len(rows))
	for _, row := range rows {
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

type MarkReadInput struct {
	Title      string   `json:"title" jsonschema:"book title; call hardcover_search first if unsure"`
	Stars      *float64 `json:"stars,omitempty" jsonschema:"rating from 0.5 to 5"`
	FinishedAt *string  `json:"finished_at,omitempty" jsonschema:"finish date as YYYY-MM-DD; omit to leave the book read with no date"`
	Year       *int     `json:"year,omitempty" jsonschema:"publication year, to disambiguate a title"`
}

type MarkReadOutput struct {
	OK         bool    `json:"ok"`
	Matched    string  `json:"matched"`
	BookID     int64   `json:"book_id"`
	Created    bool    `json:"created"`
	FinalState State   `json:"final_state"`
	DateError  *string `json:"date_error"`
}

func (c *Client) markReadTool(ctx context.Context, _ *mcp.CallToolRequest, in MarkReadInput) (*mcp.CallToolResult, MarkReadOutput, error) {
	if in.Stars != nil && (*in.Stars < 0.5 || *in.Stars > 5) {
		return nil, MarkReadOutput{}, fmt.Errorf("stars must be between 0.5 and 5")
	}
	if in.FinishedAt != nil {
		if err := validateDate(*in.FinishedAt); err != nil {
			return nil, MarkReadOutput{}, err
		}
	}
	book, userBookID, created, err := c.ensureUserBook(ctx, in.Title, in.Year, "read", in.Stars)
	if err != nil {
		return nil, MarkReadOutput{}, fmt.Errorf("%w; call hardcover_search", err)
	}
	object := "status_id: 3"
	if in.Stars != nil {
		object += fmt.Sprintf(", rating: %v", *in.Stars)
	}
	if err := c.updateUserBook(ctx, userBookID, object); err != nil {
		return nil, MarkReadOutput{}, err
	}
	var dateErr error
	if in.FinishedAt != nil {
		dateErr = c.setReadDate(ctx, userBookID, *in.FinishedAt)
	} else if created {
		dateErr = c.clearAutoReads(ctx, userBookID)
	}
	state, err := c.state(ctx, userBookID)
	if err != nil {
		return nil, MarkReadOutput{}, err
	}
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
		message := dateErr.Error()
		out.DateError = &message
	}
	return nil, out, nil
}

type SetStatusInput struct {
	Title  string `json:"title" jsonschema:"book title; call hardcover_search first if unsure"`
	Status string `json:"status" jsonschema:"shelf: want, reading, read, paused, dnf or ignored"`
	Year   *int   `json:"year,omitempty" jsonschema:"publication year, to disambiguate a title"`
}

type SetStatusOutput struct {
	OK         bool    `json:"ok"`
	Matched    string  `json:"matched"`
	Status     string  `json:"status"`
	Created    bool    `json:"created"`
	FinalState State   `json:"final_state"`
	DateError  *string `json:"date_error"`
}

func (c *Client) setStatusTool(ctx context.Context, _ *mcp.CallToolRequest, in SetStatusInput) (*mcp.CallToolResult, SetStatusOutput, error) {
	statusID, ok := statusIDs[in.Status]
	if !ok {
		return nil, SetStatusOutput{}, fmt.Errorf("status must be one of %s", strings.Join(statusNames, ", "))
	}
	book, userBookID, created, err := c.ensureUserBook(ctx, in.Title, in.Year, in.Status, nil)
	if err != nil {
		return nil, SetStatusOutput{}, fmt.Errorf("%w; call hardcover_search", err)
	}
	if err := c.updateUserBook(ctx, userBookID, fmt.Sprintf("status_id: %d", statusID)); err != nil {
		return nil, SetStatusOutput{}, err
	}
	var dateErr error
	if created && in.Status == "read" {
		dateErr = c.clearAutoReads(ctx, userBookID)
	}
	state, err := c.state(ctx, userBookID)
	if err != nil {
		return nil, SetStatusOutput{}, err
	}
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

type SetRatingInput struct {
	Title string  `json:"title" jsonschema:"book title; call hardcover_search first if unsure"`
	Stars float64 `json:"stars" jsonschema:"rating from 0.5 to 5"`
	Year  *int    `json:"year,omitempty" jsonschema:"publication year, to disambiguate a title"`
}

type SetRatingOutput struct {
	OK        bool     `json:"ok"`
	Matched   string   `json:"matched"`
	Rating    *float64 `json:"rating"`
	ReadError *string  `json:"read_error,omitempty"`
}

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
	if userBookID == nil {
		return nil, SetRatingOutput{}, fmt.Errorf("%q is not on a shelf; call hardcover_mark_read or hardcover_set_status first", book.Title)
	}
	if err := c.updateUserBook(ctx, *userBookID, fmt.Sprintf("rating: %v", in.Stars)); err != nil {
		return nil, SetRatingOutput{}, err
	}
	state, err := c.state(ctx, *userBookID)
	if err != nil {
		return nil, SetRatingOutput{}, err
	}
	ok := state.Rating != nil && *state.Rating == in.Stars
	return nil, SetRatingOutput{
		OK:        ok,
		Matched:   fmt.Sprintf("%s (%s)", book.Title, book.Author),
		Rating:    state.Rating,
		ReadError: state.ReadError,
	}, nil
}

type StatsInput struct{}

type StatsOutput struct {
	Total                 int            `json:"total"`
	ByStatusID            map[string]int `json:"by_status_id" jsonschema:"user_book counts keyed by status id"`
	Read                  int            `json:"read"`
	Rated                 int            `json:"rated"`
	AverageRating         *float64       `json:"average_rating"`
	ReadBooksMissingDates int            `json:"read_books_missing_dates" jsonschema:"read books with neither a first nor a last read date"`
}

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
	var read, rated, missing int
	var sum float64
	for _, row := range rows {
		byStatus[strconv.Itoa(row.StatusID)]++
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
		average := math.Round(sum/float64(rated)*100) / 100
		out.AverageRating = &average
	}
	return nil, out, nil
}

func containsDate(dates []*string, want string) bool {
	for _, date := range dates {
		if date != nil && *date == want {
			return true
		}
	}
	return false
}
