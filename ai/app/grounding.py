"""Checks that a value a model returned is really in the text it was given.

The resume parser uses these to drop anything the resume does not carry
(app/extract.py), the fake provider and the seed generator use
`standards_named` to read the GAAP exposure straight off the page, and the
fixture tests use them to keep the expected outputs honest.
"""

from __future__ import annotations

import re

from app import taxonomy

_DASHES = str.maketrans({"\u2013": "-", "\u2014": "-", "\u2019": "'", "\u2018": "'"})

# An accounting framework or a numbered standard, as resumes write them:
# "US GAAP", "UK GAAP", "IFRS", "IFRS 17", "IAS 36", "ASC 606", "ASC 350-40",
# "FRS 102", "GASB 87".
_STANDARD = re.compile(
    r"(?<![A-Za-z0-9])(?:"
    r"(?<![Nn]on-)(?<![Nn]on )(?:US |U\.S\. |UK |Canadian )?GAAP"
    r"|(?:ASC|IAS|FRS|GASB|ASPE) \d+(?:-\d+)?"
    r"|IFRS(?: \d+)?|GASB|ASPE"
    r")(?![A-Za-z0-9])"
)


def normalize(text: str) -> str:
    """Lower case, single spaces, plain dashes and apostrophes."""
    return " ".join(text.translate(_DASHES).split()).lower()


def in_text(fragment: str, text: str) -> bool:
    """True when the fragment appears in the text as whole words, ignoring case,
    line breaks and dash style: "IAS 1" is not in "IAS 16", nor "R" in "QuickBooks"."""
    needle = normalize(fragment)
    return bool(needle) and re.search(rf"(?<!\w){re.escape(needle)}(?!\w)", normalize(text)) is not None


# Characters a model retypes when it copies a passage: any one of a group stands for the others.
_INTERCHANGEABLE = ("-\u2013\u2014", "'\u2018\u2019", '"\u201c\u201d')


def _as_written(char: str) -> str:
    """A pattern for one character of a quote, however the page writes it."""
    for group in _INTERCHANGEABLE:
        if char in group:
            return f"[{group}]"
    return re.escape(char)


def find_quote(quote: str, text: str) -> str | None:
    """The passage of the text that the quote reproduces, as the text itself
    writes it; None when the text has no such passage.

    A quote may differ from the page only in case, line breaks and runs of
    spaces, and the style of dashes, apostrophes and quotation marks. It must
    say something (punctuation alone is not a quote) and start and end on
    whole words ("R" is not a quote from "QuickBooks"). What comes back is
    cut from the text, so it is always a substring of it."""
    words = quote.split()
    if not re.search(r"\w", quote):
        return None
    pattern = r"\s+".join("".join(_as_written(c) for c in word) for word in words)
    if re.match(r"\w", words[0]):
        pattern = r"(?<!\w)" + pattern
    if re.search(r"\w$", words[-1]):
        pattern += r"(?!\w)"
    found = re.search(pattern, text, re.IGNORECASE)
    return found.group() if found else None


_WORD = r"(?<![A-Za-z0-9]){}(?![A-Za-z0-9])"
_CASE_SENSITIVE_UP_TO = 4  # "CA", "EA", "SQL": short enough to be an ordinary word in lower case


def mention_of(text: str, term: taxonomy.Term) -> str | None:
    """How the text names the term, by label or alias, as the text writes it;
    None when it does not. Short aliases ("CA", "EA", "SQL") are matched
    case-sensitively so "California" does not count as a Chartered
    Accountant. A body-specific qualification is also named by the letters it
    shares: "CPA" names a US CPA."""
    for name in (term.label, *term.aliases, *term.inherited):
        pattern = _WORD.format(re.escape(name))
        flags = 0 if len(name) <= _CASE_SENSITIVE_UP_TO else re.IGNORECASE
        if found := re.search(pattern, text, flags):
            return found.group()
    return None


def mentions(text: str, term: taxonomy.Term) -> bool:
    """True when the text names the term (see `mention_of`)."""
    return mention_of(text, term) is not None


def line_with(fragment: str, text: str) -> str | None:
    """The first line of the text that carries the fragment, trimmed of bullets; None if no line does."""
    for line in text.splitlines():
        if in_text(fragment, line):
            return line.strip().lstrip("-*\u2022> \t") or None
    return None


def year_in_text(year: int, text: str) -> bool:
    """True when the text writes the year: in full, as '23, or as month/year (06/23).
    The day of a date (10/23/2019) is not a year."""
    short = f"{year % 100:02d}"
    pattern = rf"(?<!\d)(?:{year}|['\u2019]{short}|(?<![\d/])(?:0?[1-9]|1[0-2])/{short}(?![\d/]))(?!\d)"
    return re.search(pattern, text) is not None


def standards_named(text: str) -> list[str]:
    """The accounting frameworks and standards the text names, in order of first mention."""
    found: list[str] = []
    seen: set[str] = set()
    for match in _STANDARD.finditer(text):
        name = match.group()
        if name.lower() not in seen:
            seen.add(name.lower())
            found.append(name)
    return found
