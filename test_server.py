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


if __name__ == "__main__":
    for name, fn in sorted(globals().items()):
        if name.startswith("test_"):
            fn()
            print(f"{name}: OK")
    print("all passed")
