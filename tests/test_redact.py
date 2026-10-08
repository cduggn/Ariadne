from agent.redact import MASK, redact


def test_common_secret_shapes_are_masked():
    samples = ["AKIAABCDEFGHIJKLMNOP", "password=hunter2", "DB_PASSWORD: s3cr3t!", "Authorization: Bearer abcdefghijklmnopqrstuv",
               "postgres://app:pa55word@db:5432/x", "token='ghp_abcdefghijklmnopqrstuvwxyz0123'", "api_key = sk-abcdefghijklmnopqrstuvwxyz"]
    for s in samples:
        assert MASK in redact(s), s


def test_ordinary_text_is_left_alone():
    s = 'FATAL: DATABASE_URL is not set'
    assert redact(s) == s


def test_fixtures_contain_no_unredacted_secret_shapes():
    import pathlib
    import re
    pat = re.compile(r"AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|BEGIN [A-Z ]*PRIVATE KEY")
    for p in pathlib.Path("fixtures").rglob("*.json"):
        assert not pat.search(p.read_text()), p
