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


_WORD = r"(?<![A-Za-z0-9]){}(?![A-Za-z0-9])"
_CASE_SENSITIVE_UP_TO = 4  # "CA", "EA", "SQL": short enough to be an ordinary word in lower case


def mentions(text: str, term: taxonomy.Term) -> bool:
    """True when the text names the term by label or alias. Short aliases
    ("CA", "EA", "SQL") are matched case-sensitively so "California" does not
    count as a Chartered Accountant. A body-specific qualification is also
    named by the letters it shares: "CPA" names a US CPA."""
    for name in (term.label, *term.aliases, *term.inherited):
        pattern = _WORD.format(re.escape(name))
        flags = 0 if len(name) <= _CASE_SENSITIVE_UP_TO else re.IGNORECASE
        if re.search(pattern, text, flags):
            return True
    return False


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
