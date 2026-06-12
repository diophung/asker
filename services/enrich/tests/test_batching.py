"""Batching math: chars/4 token estimate, <=3500 tokens AND <=16 texts per call."""

from enrich.embedder import MAX_BATCH_TEXTS, MAX_BATCH_TOKENS, estimate_tokens, plan_batches


def test_estimate_tokens_chars_over_four_rounded_up():
    assert estimate_tokens("") == 1  # floor of one token, never zero
    assert estimate_tokens("abc") == 1
    assert estimate_tokens("abcd") == 1
    assert estimate_tokens("abcde") == 2
    assert estimate_tokens("x" * 2048) == 512  # one standard chunk ~512 tokens


def test_empty_input_plans_no_batches():
    assert plan_batches([]) == []


def test_single_batch_under_both_limits():
    texts = ["x" * 100] * 10  # 10 texts, 250 estimated tokens
    assert plan_batches(texts) == [list(range(10))]


def test_splits_on_max_texts():
    texts = ["hi"] * (MAX_BATCH_TEXTS + 1)  # tiny texts: only the count limit binds
    batches = plan_batches(texts)
    assert batches == [list(range(MAX_BATCH_TEXTS)), [MAX_BATCH_TEXTS]]


def test_splits_on_token_budget():
    # Each text estimates to 512 tokens; 6 * 512 = 3072 fits, 7 * 512 = 3584 > 3500.
    texts = ["x" * 2048] * 8
    batches = plan_batches(texts)
    assert batches == [[0, 1, 2, 3, 4, 5], [6, 7]]
    for batch in batches:
        assert sum(estimate_tokens(texts[i]) for i in batch) <= MAX_BATCH_TOKENS


def test_oversize_single_text_ships_alone():
    huge = "x" * (MAX_BATCH_TOKENS * 4 + 100)  # alone exceeds the budget; cannot split
    texts = ["small", huge, "small"]
    assert plan_batches(texts) == [[0], [1], [2]]


def test_indices_cover_every_text_in_order():
    texts = ["x" * n for n in (1, 5000, 9000, 3, 14001, 2)]
    batches = plan_batches(texts)
    flat = [i for batch in batches for i in batch]
    assert flat == list(range(len(texts)))
    for batch in batches:
        assert len(batch) <= MAX_BATCH_TEXTS


def test_custom_limits():
    texts = ["aaaa", "bbbb", "cccc"]  # 1 token each
    assert plan_batches(texts, max_tokens=2, max_texts=16) == [[0, 1], [2]]
    assert plan_batches(texts, max_tokens=100, max_texts=1) == [[0], [1], [2]]
