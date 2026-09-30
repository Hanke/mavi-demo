"""Hand-written parser fixtures under infra/fixtures.

A small set of sample resumes (plain text plus a PDF rendered from that text)
and job descriptions, each with the structured output the parser is expected
to produce, written by hand. The parser tests, the pipeline tests and the eval
all read them through this module, so there is one definition of "correct".

Layout, all under infra/fixtures:

    resumes/<slug>.txt            the resume as plain text (what /parse-resume gets)
    resumes/<slug>.pdf            the same text as a PDF (`python -m app.fixtures render`)
    resumes/<slug>.expected.json  {"$comment", "contact", "profile": CandidateProfile}
    jds/<slug>.txt                the job description
    jds/<slug>.expected.json      {"$comment", "company", "requirements": RoleRequirements,
                                   "hard_filter_matches": [resume slugs]}

The PDF is a pure function of the text (no timestamps, no ids), so the
committed file is checked byte for byte against a re-render by the tests and
cannot drift from the text.
"""

from __future__ import annotations

import json
import sys
import textwrap
from dataclasses import dataclass
from datetime import date
from pathlib import Path
from typing import Any, cast

from pydantic import BaseModel, ConfigDict

from app.schemas import CandidateProfile, RoleRequirements

ROOT = Path(__file__).resolve().parents[2] / "infra" / "fixtures"
RESUMES_DIR = ROOT / "resumes"
JDS_DIR = ROOT / "jds"
EXPECTED_SUFFIX = ".expected.json"

# The day the expected outputs were written. `years_experience` runs from the
# first professional role to this date, so a parser test passes it as "today"
# rather than letting the values drift a year every September.
AS_OF = date(2026, 9, 30)


class Contact(BaseModel):
    """Header details a resume parser returns alongside the profile."""

    model_config = ConfigDict(extra="forbid")

    full_name: str
    email: str
    phone: str | None = None
    location: str | None = None


@dataclass(frozen=True)
class ResumeFixture:
    slug: str
    text: str
    pdf_path: Path
    contact: Contact
    expected: CandidateProfile
    comment: str
    raw: dict[str, Any]
    """The expected JSON exactly as committed."""


@dataclass(frozen=True)
class JDFixture:
    slug: str
    text: str
    company: str
    expected: RoleRequirements
    hard_filter_matches: list[str]
    """Resume slugs whose expected profile satisfies this JD's certification and software requirements."""
    comment: str
    raw: dict[str, Any]


def _read_expected(path: Path) -> dict[str, Any]:
    return cast(dict[str, Any], json.loads(path.read_text(encoding="utf-8")))


def _slugs(directory: Path) -> list[str]:
    return sorted(p.name[: -len(EXPECTED_SUFFIX)] for p in directory.glob(f"*{EXPECTED_SUFFIX}"))


def resume_slugs(root: Path = ROOT) -> list[str]:
    """Slugs only (a directory listing), so a test can parametrize without loading anything."""
    return _slugs(root / "resumes")


def jd_slugs(root: Path = ROOT) -> list[str]:
    return _slugs(root / "jds")


def load_resume(slug: str, root: Path = ROOT) -> ResumeFixture:
    directory = root / "resumes"
    raw = _read_expected(directory / f"{slug}{EXPECTED_SUFFIX}")
    return ResumeFixture(
        slug=slug,
        text=(directory / f"{slug}.txt").read_text(encoding="utf-8"),
        pdf_path=directory / f"{slug}.pdf",
        contact=Contact.model_validate(raw["contact"]),
        expected=CandidateProfile.model_validate(raw["profile"]),
        comment=cast(str, raw.get("$comment", "")),
        raw=raw,
    )


def load_jd(slug: str, root: Path = ROOT) -> JDFixture:
    directory = root / "jds"
    raw = _read_expected(directory / f"{slug}{EXPECTED_SUFFIX}")
    return JDFixture(
        slug=slug,
        text=(directory / f"{slug}.txt").read_text(encoding="utf-8"),
        company=cast(str, raw["company"]),
        expected=RoleRequirements.model_validate(raw["requirements"]),
        hard_filter_matches=list(cast(list[str], raw.get("hard_filter_matches", []))),
        comment=cast(str, raw.get("$comment", "")),
        raw=raw,
    )


def load_resumes(root: Path = ROOT) -> list[ResumeFixture]:
    return [load_resume(slug, root) for slug in resume_slugs(root)]


def load_jds(root: Path = ROOT) -> list[JDFixture]:
    return [load_jd(slug, root) for slug in jd_slugs(root)]


def passes_hard_filters(profile: CandidateProfile, req: RoleRequirements) -> bool:
    """The certification and software containment of the README's shortlist query.

    Only the two array clauses. The query's `available_from <= starts_on` clause
    depends on the availability date the pipeline derives, which a profile does
    not carry, so `hard_filter_matches` is deliberately computed without it.
    """
    return set(req.required_certifications) <= set(profile.certifications) and set(req.required_software) <= set(
        profile.software
    )


# --- PDF rendering -----------------------------------------------------------
#
# A minimal single-column PDF: Helvetica body, bold first line, WinAnsi text so
# accents and the pound sign survive. Enough for a text extractor to give the
# .txt back; it is not trying to look like a designed resume.

PAGE_WIDTH = 612
PAGE_HEIGHT = 792
MARGIN = 54
BODY_SIZE = 10
BODY_LEADING = 12.5
TITLE_SIZE = 14
TITLE_LEADING = 18
MAX_CHARS = 95
MIN_WRAP_WIDTH = 20  # a line indented nearly to the margin still wraps instead of raising
LINES_PER_PAGE = int((PAGE_HEIGHT - 2 * MARGIN) / BODY_LEADING)
ENCODING = "cp1252"


def _escape(text: str) -> str:
    return text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")


def wrap_line(line: str) -> list[str]:
    """Greedy word wrap; continuation lines hang two spaces past the line's indentation.

    A token wider than the line (a long email or URL) overflows the margin rather
    than being split, so a text extractor still gives it back verbatim."""
    line = line.expandtabs(4).rstrip()
    if len(line) <= MAX_CHARS:
        return [line]
    indent = len(line) - len(line.lstrip(" "))
    wrapped = textwrap.wrap(
        line.lstrip(" "),
        width=max(MAX_CHARS - indent, MIN_WRAP_WIDTH),
        subsequent_indent="  ",
        break_on_hyphens=False,
        break_long_words=False,
    )
    return [" " * indent + w for w in wrapped]


def _page_stream(lines: list[str], first_page: bool) -> bytes:
    ops: list[str] = ["BT"]
    y = PAGE_HEIGHT - MARGIN - BODY_SIZE
    ops.append(f"{MARGIN} {y} Td")
    body = lines
    if first_page and lines:
        ops.append(f"/F2 {TITLE_SIZE} Tf ({_escape(lines[0])}) Tj 0 -{TITLE_LEADING} Td")
        body = lines[1:]
    ops.append(f"/F1 {BODY_SIZE} Tf {BODY_LEADING} TL")
    ops.extend(f"({_escape(line)}) Tj T*" if line else "T*" for line in body)
    ops.append("ET")
    return "\n".join(ops).encode(ENCODING)


def render_pdf(text: str) -> bytes:
    """Render plain text to a deterministic PDF. Raises UnicodeEncodeError for
    characters outside cp1252, which the fixtures avoid."""
    lines = [wrapped for raw in text.rstrip("\n").split("\n") for wrapped in wrap_line(raw)]
    pages = [lines[i : i + LINES_PER_PAGE] for i in range(0, len(lines), LINES_PER_PAGE)]  # never empty: "" is [""]

    objects: list[bytes] = []  # index i holds object i + 1

    def add(body: bytes) -> int:
        objects.append(body)
        return len(objects)

    catalog = add(b"")  # filled in once the pages object number is known
    pages_obj = add(b"")
    body_font = add(b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
    title_font = add(b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
    page_numbers: list[int] = []
    for i, page in enumerate(pages):
        stream = _page_stream(page, first_page=i == 0)
        content = add(b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream")
        page_numbers.append(
            add(
                (
                    f"<< /Type /Page /Parent {pages_obj} 0 R /MediaBox [0 0 {PAGE_WIDTH} {PAGE_HEIGHT}] "
                    f"/Resources << /Font << /F1 {body_font} 0 R /F2 {title_font} 0 R >> >> "
                    f"/Contents {content} 0 R >>"
                ).encode("ascii")
            )
        )
    objects[catalog - 1] = f"<< /Type /Catalog /Pages {pages_obj} 0 R >>".encode("ascii")
    kids = " ".join(f"{n} 0 R" for n in page_numbers)
    objects[pages_obj - 1] = f"<< /Type /Pages /Kids [{kids}] /Count {len(page_numbers)} >>".encode("ascii")

    out = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets: list[int] = []
    for number, body in enumerate(objects, start=1):
        offsets.append(len(out))
        out += b"%d 0 obj\n" % number + body + b"\nendobj\n"
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objects) + 1)
    for offset in offsets:
        out += b"%010d 00000 n \n" % offset
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objects) + 1, xref)
    return bytes(out)


def render_all(root: Path = ROOT) -> dict[Path, bytes]:
    """PDF path -> bytes for every resume text, whether or not the PDF exists yet."""
    directory = root / "resumes"
    return {
        directory / f"{slug}.pdf": render_pdf((directory / f"{slug}.txt").read_text(encoding="utf-8"))
        for slug in _slugs(directory)
    }


def main(argv: list[str]) -> int:
    if argv[1:] != ["render"]:
        print("usage: python -m app.fixtures render", file=sys.stderr)
        return 2
    for path, content in render_all().items():
        path.write_bytes(content)
        print(f"wrote {path.relative_to(ROOT.parents[1])}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
