package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const canned = `{"data":{"search":{"results":{"hits":[
	{"document":{"id":"373525","title":"It","author_names":["Stephen King"]}},
	{"document":{"id":1110269,"title":"It: Volume 1","author_names":["Stephen King"]}}
]}}}}`

func me(userBooks string) string {
	return `{"data":{"me":[{"user_books":` + userBooks + `}]}}`
}

const meEmpty = `{"data":{"me":[{"user_books":[]}]}}`

type rule struct {
	needle   string
	response string
}

// scripted returns a client whose first rule containing the query wins, and a
// log of every query sent.
func scripted(t *testing.T, rules ...rule) (*Client, *[]string) {
	t.Helper()
	sent := []string{}
	c := &Client{TokenPath: "unused"}
	c.Do = func(_ context.Context, query string, _ map[string]any) ([]byte, error) {
		sent = append(sent, query)
		for _, r := range rules {
			if strings.Contains(query, r.needle) {
				return []byte(r.response), nil
			}
		}
		t.Fatalf("unscripted query: %s", query)
		return nil, nil
	}
	return c, &sent
}

func TestSearchSendsTheTitleAsAVariableNotInterpolated(t *testing.T) {
	var query string
	var variables map[string]any
	c := &Client{Do: func(_ context.Context, q string, vars map[string]any) ([]byte, error) {
		query, variables = q, vars
		return []byte(canned), nil
	}}
	if _, err := c.Search(context.Background(), `O'Brien "quoted"`, 3); err != nil {
		t.Fatal(err)
	}
	if variables["q"] != `O'Brien "quoted"` || variables["n"] != 3 {
		t.Fatalf("variables = %v", variables)
	}
	if !strings.Contains(query, "$q") || !strings.Contains(query, "$n") {
		t.Fatalf("query does not use variables: %s", query)
	}
}

func TestStringBookIDsAreCoercedToInt(t *testing.T) {
	c, _ := scripted(t, rule{"search(query: $q", canned})
	book, err := c.findBook(context.Background(), "It", nil)
	if err != nil {
		t.Fatal(err)
	}
	if book.BookID != 373525 {
		t.Fatalf("book_id = %d", book.BookID)
	}
}

func TestExactTitleWinsOverALongerMatch(t *testing.T) {
	c, _ := scripted(t, rule{"search(query: $q", canned})
	exact, err := c.findBook(context.Background(), "It", nil)
	if err != nil {
		t.Fatal(err)
	}
	longer, err := c.findBook(context.Background(), "It: Volume 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if exact.Title != "It" || longer.BookID != 1110269 {
		t.Fatalf("exact = %q, longer = %d", exact.Title, longer.BookID)
	}
}

func TestNoExactTitleFallsBackToTheFirstCandidate(t *testing.T) {
	c, _ := scripted(t, rule{"search(query: $q", canned})
	book, err := c.findBook(context.Background(), "Something Else", nil)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "It" {
		t.Fatalf("title = %q", book.Title)
	}
}

func TestSearchReportsAnErrorInsteadOfAnEmptyResult(t *testing.T) {
	c := &Client{Do: func(context.Context, string, map[string]any) ([]byte, error) {
		return []byte(`{"errors":[{"message":"boom"}]}`), nil
	}}
	if _, err := c.Search(context.Background(), "x", 5); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestMarkReadWithoutADateClearsTheRowInsertUserBookStamped(t *testing.T) {
	c, sent := scripted(t,
		rule{"search(query: $q", canned},
		rule{"book_id: {_eq: 373525}", meEmpty},
		rule{"insert_user_book(object", `{"data":{"insert_user_book":{"id":42,"error":null}}}`},
		rule{"update_user_book(id", `{"data":{"update_user_book":{"id":42,"error":null}}}`},
		rule{"status_id rating last_read_date", me(`[{"status_id":3,"rating":null,"last_read_date":null,"user_book_reads":[]}]`)},
		rule{"user_book_reads { id finished_at }", me(`[{"user_book_reads":[{"id":7}]}]`)},
		rule{"delete_user_book_read", `{"data":{"delete_user_book_read":{"id":7}}}`},
	)
	_, out, err := c.markReadTool(context.Background(), nil, MarkReadInput{Title: "It"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.DateError != nil || out.FinalState.ReadRows != 0 {
		t.Fatalf("out = %+v", out)
	}
	if !containsQuery(*sent, "delete_user_book_read(id: 7)") {
		t.Fatalf("auto read row not deleted: %v", *sent)
	}
	if containsQuery(*sent, "insert_user_book_read") {
		t.Fatalf("unexpected read row insert: %v", *sent)
	}
}

func TestMarkReadWithADateCreatesOneReadRowAndConfirmsIt(t *testing.T) {
	c, sent := scripted(t,
		rule{"search(query: $q", canned},
		rule{"book_id: {_eq: 373525}", me(`[{"id":42}]`)},
		rule{"update_user_book(id", `{"data":{"update_user_book":{"id":42,"error":null}}}`},
		rule{"status_id rating last_read_date", me(`[{"status_id":3,"rating":null,"last_read_date":"2026-09-10","user_book_reads":[{"id":9,"finished_at":"2026-09-10"}]}]`)},
		rule{"user_book_reads { id finished_at }", me(`[{"user_book_reads":[]}]`)},
		rule{"insert_user_book_read", `{"data":{"insert_user_book_read":{"id":9}}}`},
		rule{"user_books(where: {id: {_eq: 42}}) { last_read_date }", me(`[{"last_read_date":"2026-09-10"}]`)},
	)
	date := "2026-09-10"
	_, out, err := c.markReadTool(context.Background(), nil, MarkReadInput{Title: "It", FinishedAt: &date})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.DateError != nil {
		t.Fatalf("out = %+v", out)
	}
	if len(out.FinalState.ReadDates) != 1 || out.FinalState.ReadDates[0] == nil || *out.FinalState.ReadDates[0] != date {
		t.Fatalf("read dates = %v", out.FinalState.ReadDates)
	}
	if !containsQuery(*sent, `insert_user_book_read(user_book_id: 42`) || !containsQuery(*sent, `"2026-09-10"`) {
		t.Fatalf("read row not inserted with the date: %v", *sent)
	}
	if containsQuery(*sent, "delete_user_book_read") {
		t.Fatalf("unexpected read row delete: %v", *sent)
	}
}

func TestMarkReadRejectsAMalformedDate(t *testing.T) {
	c, sent := scripted(t)
	date := "10/09/2026"
	if _, _, err := c.markReadTool(context.Background(), nil, MarkReadInput{Title: "It", FinishedAt: &date}); err == nil {
		t.Fatal("expected a date error")
	}
	if len(*sent) != 0 {
		t.Fatalf("queries sent: %v", *sent)
	}
}

func TestSetStatusReadClearsTheAutomaticDateRow(t *testing.T) {
	c, sent := scripted(t,
		rule{"search(query: $q", canned},
		rule{"book_id: {_eq: 373525}", meEmpty},
		rule{"insert_user_book(object", `{"data":{"insert_user_book":{"id":42,"error":null}}}`},
		rule{"update_user_book(id", `{"data":{"update_user_book":{"id":42,"error":null}}}`},
		rule{"status_id rating last_read_date", me(`[{"status_id":3,"rating":null,"last_read_date":null,"user_book_reads":[]}]`)},
		rule{"user_book_reads { id finished_at }", me(`[{"user_book_reads":[{"id":7}]}]`)},
		rule{"delete_user_book_read", `{"data":{"delete_user_book_read":{"id":7}}}`},
	)
	_, out, err := c.setStatusTool(context.Background(), nil, SetStatusInput{Title: "It", Status: "read"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.DateError != nil || out.FinalState.ReadRows != 0 {
		t.Fatalf("out = %+v", out)
	}
	if !containsQuery(*sent, "delete_user_book_read(id: 7)") {
		t.Fatalf("auto read row not deleted: %v", *sent)
	}
}

func TestSetRatingUpdatesABookThatIsOnAShelf(t *testing.T) {
	c, sent := scripted(t,
		rule{"search(query: $q", canned},
		rule{"book_id: {_eq: 373525}", me(`[{"id":42}]`)},
		rule{"update_user_book(id", `{"data":{"update_user_book":{"id":42,"error":null}}}`},
		rule{"status_id rating last_read_date", me(`[{"status_id":3,"rating":4.5,"last_read_date":null,"user_book_reads":[]}]`)},
	)
	_, out, err := c.setRatingTool(context.Background(), nil, SetRatingInput{Title: "It", Stars: 4.5})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Rating == nil || *out.Rating != 4.5 {
		t.Fatalf("out = %+v", out)
	}
	if containsQuery(*sent, "insert_user_book") {
		t.Fatalf("unexpected insert: %v", *sent)
	}
}

func TestSetRatingRefusesABookThatIsNotOnAShelf(t *testing.T) {
	c, sent := scripted(t,
		rule{"search(query: $q", canned},
		rule{"book_id: {_eq: 373525}", meEmpty},
	)
	_, _, err := c.setRatingTool(context.Background(), nil, SetRatingInput{Title: "It", Stars: 4.5})
	if err == nil || !strings.Contains(err.Error(), "not on a shelf") {
		t.Fatalf("err = %v", err)
	}
	if containsQuery(*sent, "update_user_book(id") {
		t.Fatalf("unexpected update: %v", *sent)
	}
}

func TestLibraryListsShelfContents(t *testing.T) {
	c, _ := scripted(t, rule{"order_by: {last_read_date: desc_nulls_last}", me(`[
		{"id":42,"rating":4.5,"first_read_date":"2020-01-01","last_read_date":"2026-09-10",
		 "book":{"title":"It","contributions":[{"author":{"name":"Stephen King"}}]}}
	]`)})
	_, out, err := c.libraryTool(context.Background(), nil, LibraryInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "read" || out.Count != 1 || out.Books[0].Author != "Stephen King" {
		t.Fatalf("out = %+v", out)
	}
	if out.Books[0].FinishedAt == nil || *out.Books[0].FinishedAt != "2026-09-10" {
		t.Fatalf("finished_at = %v", out.Books[0].FinishedAt)
	}
}

func TestStatsCountByStatusAndTheMissingDateGap(t *testing.T) {
	c, _ := scripted(t, rule{"user_books { status_id rating", me(`[
		{"status_id":1,"rating":null,"first_read_date":null,"last_read_date":null},
		{"status_id":3,"rating":4,"first_read_date":"2020-01-01","last_read_date":"2026-09-10"},
		{"status_id":3,"rating":null,"first_read_date":null,"last_read_date":null},
		{"status_id":5,"rating":null,"first_read_date":null,"last_read_date":null}
	]`)})
	_, out, err := c.statsTool(context.Background(), nil, StatsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 4 || out.Read != 2 || out.Rated != 1 || out.ReadBooksMissingDates != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.ByStatusID["3"] != 2 || out.ByStatusID["5"] != 1 {
		t.Fatalf("by_status_id = %v", out.ByStatusID)
	}
	if out.AverageRating == nil || *out.AverageRating != 4 {
		t.Fatalf("average_rating = %v", out.AverageRating)
	}
}

func TestServerListsAllToolsWithSchemas(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "hardcover", Version: "test"}, nil)
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	addTools(server, c)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 6 {
		t.Fatalf("tools = %d", len(list.Tools))
	}
	for _, tool := range list.Tools {
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", tool.Name)
		}
	}
}

func TestToolCallReturnsStructuredContent(t *testing.T) {
	c, _ := scripted(t, rule{"user_books { status_id rating", me(`[
		{"status_id":3,"rating":4.5,"first_read_date":"2020-01-01","last_read_date":"2026-09-10"}
	]`)})
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "hardcover", Version: "test"}, nil)
	addTools(server, c)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hardcover_stats", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool failed: %v", result.Content)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["read"] != 1.0 {
		t.Fatalf("structured content = %#v", result.StructuredContent)
	}
	if len(result.Content) == 0 {
		t.Fatal("no text content")
	}
}

func containsQuery(sent []string, needle string) bool {
	for _, query := range sent {
		if strings.Contains(query, needle) {
			return true
		}
	}
	return false
}
