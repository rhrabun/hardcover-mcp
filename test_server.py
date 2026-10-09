import json

import server

CANNED = {
    "data": {
        "search": {
            "results": {
                "hits": [
                    {"document": {"id": "373525", "title": "It", "author_names": ["Stephen King"]}},
                    {"document": {"id": 1110269, "title": "It: Volume 1", "author_names": ["Stephen King"]}},
                ]
            }
        }
    }
}


def test_search_sends_the_title_as_a_variable_not_interpolated():
    seen = {}

    def fake_gql(query, variables=None):
        seen["query"] = query
        seen["variables"] = variables
        return CANNED

    server._gql = fake_gql
    server._search('O\'Brien "quoted"', per_page=3)
    assert seen["variables"] == {"q": 'O\'Brien "quoted"', "n": 3}
    assert "$q" in seen["query"] and "$n" in seen["query"]


def test_string_book_ids_are_coerced_to_int():
    server._gql = lambda *a, **k: CANNED
    assert server._find_book("It")["book_id"] == 373525


def test_exact_title_wins_over_a_longer_match():
    server._gql = lambda *a, **k: CANNED
    assert server._find_book("It")["title"] == "It"
    assert server._find_book("It: Volume 1")["book_id"] == 1110269


def test_no_exact_title_falls_back_to_the_first_candidate():
    server._gql = lambda *a, **k: CANNED
    assert server._find_book("Something Else")["title"] == "It"


def test_search_raises_on_error_instead_of_returning_empty():
    server._gql = lambda *a, **k: {"errors": [{"message": "boom"}]}
    try:
        server._search("x")
    except server.HardcoverError:
        pass
    else:
        raise AssertionError("expected HardcoverError")


def ME(rows):
    """A `me { user_books }` response."""
    return {"data": {"me": [{"user_books": rows}]}}


def _scripted(rules):
    """Fake _gql: the first rule whose substring appears in the query wins; every query is logged."""
    log = []

    def fake(query, variables=None):
        log.append(query)
        for needle, response in rules:
            if needle in query:
                return response
        raise AssertionError(f"unscripted query: {query}")

    return fake, log


def test_mark_read_without_a_date_clears_the_row_insert_user_book_stamped():
    fake, log = _scripted(
        [
            ("search(query: $q", CANNED),
            ("book_id: {_eq: 373525}", ME([])),
            ("insert_user_book(object", {"data": {"insert_user_book": {"id": 42, "error": None}}}),
            ("update_user_book(id", {"data": {"update_user_book": {"id": 42, "error": None}}}),
            ("user_book_reads { id }", ME([{"user_book_reads": [{"id": 7}]}])),
            ("delete_user_book_read", {"data": {"delete_user_book_read": {"id": 7}}}),
            ("status_id rating last_read_date", ME([{"status_id": 3, "rating": None, "last_read_date": None, "user_book_reads": []}])),
        ]
    )
    server._gql = fake
    out = json.loads(server.hardcover_mark_read("It"))
    assert out["ok"] is True and out["date_error"] is None
    assert out["final_state"]["read_rows"] == 0
    assert any("delete_user_book_read(id: 7)" in q for q in log)
    assert not any("insert_user_book_read" in q for q in log)


def test_mark_read_with_a_date_creates_one_read_row_and_confirms_it():
    fake, log = _scripted(
        [
            ("search(query: $q", CANNED),
            ("book_id: {_eq: 373525}", ME([{"id": 42, "status_id": 3, "rating": None}])),
            ("update_user_book(id", {"data": {"update_user_book": {"id": 42, "error": None}}}),
            ("user_book_reads { id }", ME([{"user_book_reads": []}])),
            ("insert_user_book_read", {"data": {"insert_user_book_read": {"id": 9}}}),
            ("user_books(where: {id: {_eq: 42}}) { last_read_date }", ME([{"last_read_date": "2026-09-10"}])),
            (
                "status_id rating last_read_date",
                ME([{"status_id": 3, "rating": None, "last_read_date": "2026-09-10",
                     "user_book_reads": [{"id": 9, "finished_at": "2026-09-10"}]}]),
            ),
        ]
    )
    server._gql = fake
    out = json.loads(server.hardcover_mark_read("It", finished_at="2026-09-10"))
    assert out["ok"] is True and out["date_error"] is None
    assert out["final_state"]["read_dates"] == ["2026-09-10"]
    assert any('insert_user_book_read(user_book_id: 42' in q and '"2026-09-10"' in q for q in log)
    assert not any("delete_user_book_read" in q for q in log)


def test_set_status_read_clears_the_automatic_date_row():
    fake, log = _scripted(
        [
            ("search(query: $q", CANNED),
            ("book_id: {_eq: 373525}", ME([])),
            ("insert_user_book(object", {"data": {"insert_user_book": {"id": 42, "error": None}}}),
            ("update_user_book(id", {"data": {"update_user_book": {"id": 42, "error": None}}}),
            ("user_book_reads { id }", ME([{"user_book_reads": [{"id": 7}]}])),
            ("delete_user_book_read", {"data": {"delete_user_book_read": {"id": 7}}}),
            ("status_id rating last_read_date", ME([{"status_id": 3, "rating": None, "last_read_date": None, "user_book_reads": []}])),
        ]
    )
    server._gql = fake
    out = json.loads(server.hardcover_set_status("It", "read"))
    assert out["ok"] is True and out["date_error"] is None
    assert out["final_state"]["read_rows"] == 0
    assert any("delete_user_book_read(id: 7)" in q for q in log)


def test_set_rating_updates_a_book_that_is_on_a_shelf():
    fake, log = _scripted(
        [
            ("search(query: $q", CANNED),
            ("book_id: {_eq: 373525}", ME([{"id": 42, "status_id": 3, "rating": None}])),
            ("update_user_book(id", {"data": {"update_user_book": {"id": 42, "error": None}}}),
            ("status_id rating last_read_date", ME([{"status_id": 3, "rating": 4.5, "last_read_date": None, "user_book_reads": []}])),
        ]
    )
    server._gql = fake
    out = json.loads(server.hardcover_set_rating("It", 4.5))
    assert out["ok"] is True and out["rating"] == 4.5
    assert not any("insert_user_book" in q for q in log)


def test_set_rating_refuses_a_book_that_is_not_on_a_shelf():
    fake, log = _scripted(
        [
            ("search(query: $q", CANNED),
            ("book_id: {_eq: 373525}", ME([])),
        ]
    )
    server._gql = fake
    out = json.loads(server.hardcover_set_rating("It", 4.5))
    assert "not on a shelf" in out["error"]
    assert not any("update_user_book(id" in q for q in log)


if __name__ == "__main__":
    for name, fn in sorted(globals().items()):
        if name.startswith("test_"):
            fn()
            print(f"{name}: OK")
    print("all passed")
