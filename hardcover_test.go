// Tests live next to the code in files ending in _test.go. They are compiled
// only by `go test`, never into the binary, and they run in the same package
// (package main), which is why they can call unexported helpers such as
// findBook and markReadTool.
//
// There is no pytest: no fixtures, no conftest, no parametrize decorators.
// A test is just a function named TestXxx taking *testing.T, and `go test ./...`
// finds and runs them. Run one with `go test -run TestName -v`.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// canned is a fixed GraphQL search response. Backticks make it a raw string:
// no escaping, newlines allowed, so JSON fixtures stay readable.
//
// The book id is a JSON *string* here on purpose; one test proves the flexInt
// type coerces it.
const canned = `{"data":{"search":{"results":{"hits":[
	{"document":{"id":"373525","title":"It","author_names":["Stephen King"]}},
	{"document":{"id":1110269,"title":"It: Volume 1","author_names":["Stephen King"]}}
]}}}}`

// me wraps a user_books JSON array in the `data.me[0]` envelope every query
// returns. A small helper like this replaces a fixture factory.
func me(userBooks string) string {
	return `{"data":{"me":[{"user_books":` + userBooks + `}]}}`
}

// meEmpty is the common "book is not on any shelf" response.
const meEmpty = `{"data":{"me":[{"user_books":[]}]}}`

// rule maps a substring of a query to the response it should get.
type rule struct {
	needle   string
	response string
}

// scripted returns a client whose first rule containing the query wins, and a
// log of every query sent.
//
// This is the Go replacement for the Python suite's `server._gql = fake`:
// instead of monkeypatching a module global, the test builds a Client with a
// stub Do function and passes it to the handler under test.
//
// `rules ...rule` is a variadic parameter: callers pass zero or more rule
// literals, exactly like *args for this one argument.
//
// *testing.T is the test handle. t.Helper marks this function as a helper so a
// failure is reported at the caller's line instead of inside scripted.
// Returning *[]string hands back a pointer to the log so the caller can see
// appends made inside the closure; returning a slice by value would not.
func scripted(t *testing.T, rules ...rule) (*Client, *[]string) {
	t.Helper()
	sent := []string{}
	c := &Client{TokenPath: "unused"}
	c.Do = func(_ context.Context, query string, _ map[string]any) ([]byte, error) {
		// Closures capture variables by reference, so this appends to the same
		// `sent` that is returned below.
		sent = append(sent, query)
		for _, r := range rules {
			// strings.Contains is Python's `in` for strings.
			if strings.Contains(query, r.needle) {
				return []byte(r.response), nil
			}
		}
		// t.Fatalf fails the test immediately (it calls runtime.Goexit). The
		// bare return below is only there to satisfy the compiler, which cannot
		// know Fatalf never returns.
		t.Fatalf("unscripted query: %s", query)
		return nil, nil
	}
	return c, &sent
}

// TestSearchSendsTheTitleAsAVariableNotInterpolated proves user text never gets
// concatenated into GraphQL.
func TestSearchSendsTheTitleAsAVariableNotInterpolated(t *testing.T) {
	// Declare first, assign inside the stub, assert afterwards. This is the
	// no-framework way to spy on a call.
	var query string
	var variables map[string]any
	c := &Client{Do: func(_ context.Context, q string, vars map[string]any) ([]byte, error) {
		query, variables = q, vars
		return []byte(canned), nil
	}}
	// `if _, err := ...` discards the successful return value; only the error
	// matters. The short declaration scopes err to this if statement.
	if _, err := c.Search(context.Background(), `O'Brien "quoted"`, 3); err != nil {
		t.Fatal(err)
	}
	// Comparing an `any` to a string is legal; it compares dynamic type and
	// value. A mismatch fails rather than panicking.
	if variables["q"] != `O'Brien "quoted"` || variables["n"] != 3 {
		t.Fatalf("variables = %v", variables)
	}
	if !strings.Contains(query, "$q") || !strings.Contains(query, "$n") {
		t.Fatalf("query does not use variables: %s", query)
	}
}

// TestStringBookIDsAreCoercedToInt covers flexInt's string branch.
func TestStringBookIDsAreCoercedToInt(t *testing.T) {
	// `_` discards the query log, which this test does not inspect.
	c, _ := scripted(t, rule{"search(query: $q", canned})
	// nil is the optional year: no year was supplied.
	book, err := c.findBook(context.Background(), "It", nil)
	if err != nil {
		t.Fatal(err)
	}
	if book.BookID != 373525 {
		t.Fatalf("book_id = %d", book.BookID)
	}
}

// TestExactTitleWinsOverALongerMatch covers the ordering rule in findBook.
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

// TestNoExactTitleFallsBackToTheFirstCandidate covers the fuzzy path.
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

// TestSearchReportsAnErrorInsteadOfAnEmptyResult proves a GraphQL error is not
// mistaken for "no results". The assertion combines both conditions: err must
// be non-nil and contain the server's message.
func TestSearchReportsAnErrorInsteadOfAnEmptyResult(t *testing.T) {
	c := &Client{Do: func(context.Context, string, map[string]any) ([]byte, error) {
		return []byte(`{"errors":[{"message":"boom"}]}`), nil
	}}
	if _, err := c.Search(context.Background(), "x", 5); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// TestMarkReadWithoutADateClearsTheRowInsertUserBookStamped is the regression
// test for the "no invented dates" rule. The rules are ordered: the first
// substring match wins, so the state query is listed before the broader
// read-rows query.
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
	// nil for the *mcp.CallToolRequest: the handler ignores it. The second
	// return value is the typed output, dereferenced implicitly by `out`.
	_, out, err := c.markReadTool(context.Background(), nil, MarkReadInput{Title: "It"})
	if err != nil {
		t.Fatal(err)
	}
	// `%+v` prints struct fields with their names, which is what makes a
	// failure readable.
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

// TestMarkReadWithADateCreatesOneReadRowAndConfirmsIt covers the two-step write.
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
	// The input field is *string, so the test takes the address of a local.
	// `&date` is what turns a value into an optional argument.
	date := "2026-09-10"
	_, out, err := c.markReadTool(context.Background(), nil, MarkReadInput{Title: "It", FinishedAt: &date})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.DateError != nil {
		t.Fatalf("out = %+v", out)
	}
	// Nil checks before the deref again: ReadDates holds pointers because a
	// missing date is meaningful.
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

// TestMarkReadRejectsAMalformedDate checks validation happens before any
// network call: the scripted client has no rules, so any query would fail.
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

// TestSetStatusReadClearsTheAutomaticDateRow is the shelf-move equivalent of
// the mark-read cleanup test.
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

// TestSetRatingUpdatesABookThatIsOnAShelf covers the happy path.
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
	// Stars is a plain float64 here, not a pointer, because it is required.
	if !out.OK || out.Rating == nil || *out.Rating != 4.5 {
		t.Fatalf("out = %+v", out)
	}
	if containsQuery(*sent, "insert_user_book") {
		t.Fatalf("unexpected insert: %v", *sent)
	}
}

// TestSetRatingRefusesABookThatIsNotOnAShelf proves the write never happens
// for an off-shelf book.
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

// TestLibraryListsShelfContents exercises nested decoding and the author join.
func TestLibraryListsShelfContents(t *testing.T) {
	c, _ := scripted(t, rule{"order_by: {last_read_date: desc_nulls_last}", me(`[
		{"id":42,"rating":4.5,"first_read_date":"2020-01-01","last_read_date":"2026-09-10",
		 "book":{"title":"It","contributions":[{"author":{"name":"Stephen King"}}]}}
	]`)})
	// LibraryInput{} is the zero value: both optional fields nil, so the
	// handler applies its "read" and "no limit" defaults.
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

// TestStatsCountByStatusAndTheMissingDateGap covers the aggregation, including
// the string keys of ByStatusID and the pointer average.
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

// TestServerListsAllToolsWithSchemas registers the tools on a real server and
// connects an in-memory client, which is the closest thing to an integration
// test without spawning a process. It catches a handler whose input or output
// schema cannot be inferred.
//
// NewInMemoryTransports returns two connected ends; server uses one, client the
// other, and no sockets or stdio are involved.
func TestServerListsAllToolsWithSchemas(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "hardcover", Version: "test"}, nil)
	addTools(server, NewClient())
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	// Connect takes the server-side transport. The first return value is a
	// server session; this test does not need it.
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	// Connecting the client completes the handshake.
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	// defer runs when the test function returns; t.Cleanup would also work.
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 6 {
		t.Fatalf("tools = %d", len(list.Tools))
	}
	// t.Errorf marks a failure but lets the loop continue, so every tool
	// missing a schema is reported in one run.
	for _, tool := range list.Tools {
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", tool.Name)
		}
	}
}

// TestToolCallReturnsStructuredContent goes through the whole SDK path: same
// in-memory pair as above, then a real tools/call. It proves Out marshals and
// validates against the inferred output schema.
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

	// Arguments is map[string]any, mirroring the JSON the client sends.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hardcover_stats", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool failed: %v", result.Content)
	}
	// A type assertion unwraps the interface. The comma-ok form reports
	// whether the dynamic type matched instead of panicking; the bare form
	// `x.(T)` panics on a mismatch.
	structured, ok := result.StructuredContent.(map[string]any)
	// JSON numbers decode as float64, so the int field 1 comes back as 1.0.
	if !ok || structured["read"] != 1.0 {
		t.Fatalf("structured content = %#v", result.StructuredContent)
	}
	if len(result.Content) == 0 {
		t.Fatal("no text content")
	}
}

// containsQuery reports whether any sent query contains the needle. It is a
// plain helper, not a test, because the name does not start with Test.
func containsQuery(sent []string, needle string) bool {
	for _, query := range sent {
		if strings.Contains(query, needle) {
			return true
		}
	}
	return false
}
