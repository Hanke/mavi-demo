"""Text out of an uploaded resume or job description: PDF or DOCX, within limits.

The file is somebody else's and nothing about it is taken on trust. What it
is is decided by its first bytes, never by its name or the content type the
client sent; it is refused when it is too big, has too many pages or yields
too much text, before any of it reaches a model, and it is read in a process
of its own that is killed when reading takes too long. Every refusal is an
`UploadError` with the HTTP status and a sentence saying what the limit is
and what the file had.

The text that comes back is everything the file carries, including text a
reader of the page would not see (white on white, a one-point font). That is
deliberate: hidden text cannot be told from visible text reliably, so the
defence against what it says is in the prompts (app/delimit.py), not here.
"""

from __future__ import annotations

import io
import multiprocessing
import re
import sys
import unicodedata
import zipfile
from dataclasses import dataclass
from multiprocessing.connection import Connection
from typing import Any, Literal
from xml.etree import ElementTree

MAX_UPLOAD_BYTES = 5 * 1024 * 1024
MAX_PDF_PAGES = 10
# A .docx is a zip, and it has no page count to cap: what is bounded instead is
# how much XML its text parts may unpack to, which is also what stops a zip bomb.
MAX_DOCX_XML_BYTES = 20 * 1024 * 1024
# What reading one file may cost, whatever is in it (see `extract_text_isolated`). A resume takes well under a second.
MAX_EXTRACT_SECONDS = 20.0
MAX_EXTRACT_MEMORY_BYTES = 1024 * 1024 * 1024

Kind = Literal["pdf", "docx"]

_PDF_MAGIC = b"%PDF-"
_ZIP_MAGIC = b"PK\x03\x04"
_OLE_MAGIC = b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"  # Word 97-2003 (.doc)
# A PDF may open with a little junk before its header; readers allow about this much.
_PDF_HEADER_WINDOW = 1024
_DOCX_BODY = "word/document.xml"
_DOCX_MARGINS = re.compile(r"^word/(?:header|footer)\d*\.xml$")
_W = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"
_MC = "{http://schemas.openxmlformats.org/markup-compatibility/2006}"
_READ_CHUNK = 64 * 1024
# Control characters other than tab and line break, the invisible Unicode "tag"
# letters, what a PDF leaves between letters without it showing (soft hyphen,
# zero-width space and joiners, byte-order mark) and lone surrogates, which
# pypdf can decode from a broken font map and which are not text: they cannot
# be encoded as UTF-8, so not sent in a response or stored.
_UNPRINTABLE = re.compile(
    "[\x00-\x08\x0b-\x1f\x7f-\x9f\xad\u200b-\u200d\u2060\ufeff\ud800-\udfff\U000e0000-\U000e007f]"
)
# Typeset ligatures ("ﬁ" for "fi"): one glyph on the page, two or three letters to a reader.
_LIGATURES = re.compile("[\ufb00-\ufb06]")


class UploadError(Exception):
    """An upload the service will not read. `status` is the HTTP status to answer with."""

    def __init__(self, status: int, message: str):
        self.status = status
        super().__init__(message)


@dataclass(frozen=True)
class Extracted:
    kind: Kind
    text: str
    pages: int | None
    """Pages of a PDF; None for a DOCX, which has none until it is laid out."""


def megabytes(size: int) -> str:
    return f"{size / (1024 * 1024):.1f} MB"


def too_large() -> UploadError:
    return UploadError(413, f"file too large: the limit is {megabytes(MAX_UPLOAD_BYTES)}")


def _unsupported(data: bytes) -> UploadError:
    if data.startswith(_OLE_MAGIC):
        return UploadError(415, "unsupported file type: legacy Word (.doc) files are not read; save it as DOCX or PDF")
    return UploadError(415, "unsupported file type: only PDF and DOCX files are accepted")


def _open_docx(data: bytes) -> zipfile.ZipFile | None:
    """The file as an open archive when it is a DOCX; None when it is some other zip, or not one at all."""
    try:
        archive = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile:
        return None
    if _DOCX_BODY in archive.namelist():
        return archive
    archive.close()
    return None


def extract_text(data: bytes, max_chars: int) -> Extracted:
    """The text of a PDF or DOCX. Raises UploadError when the file is not one,
    breaks a limit, cannot be read, or has no text in it.

    What the file is comes from its content. The zip is looked for first: a
    DOCX may well have "%PDF-" somewhere in its first kilobyte, and a PDF
    never starts with "PK". This runs in the calling process, with no limit
    on how long it takes; a server uses `extract_text_isolated`."""
    if not data:
        raise UploadError(422, "the upload is empty")
    if len(data) > MAX_UPLOAD_BYTES:
        raise too_large()
    kind: Kind
    pages: int | None = None
    if data.startswith(_ZIP_MAGIC):
        archive = _open_docx(data)
        if archive is None:
            raise _unsupported(data)
        with archive:
            kind, text = "docx", _docx_text(archive)
    elif _PDF_MAGIC in data[:_PDF_HEADER_WINDOW]:
        kind = "pdf"
        text, pages = _pdf_text(data, max_chars)
    else:
        raise _unsupported(data)
    text = _clean(text)
    if len(text) > max_chars:
        raise _too_much_text(max_chars)
    if not text:
        raise UploadError(
            422, "no text could be extracted: the file may be a scan or an image; upload one with selectable text"
        )
    return Extracted(kind=kind, text=text, pages=pages)


def _work(conn: Connection, data: bytes, max_chars: int) -> None:
    """The child's side of `extract_text_isolated`: the answer, or the refusal, down the pipe."""
    if sys.platform == "linux":
        import resource

        resource.setrlimit(resource.RLIMIT_AS, (MAX_EXTRACT_MEMORY_BYTES, MAX_EXTRACT_MEMORY_BYTES))
    try:
        out = extract_text(data, max_chars)
    except UploadError as e:
        conn.send(("refused", e.status, str(e)))
    else:
        conn.send(("ok", out.kind, out.text, out.pages))
    finally:
        conn.close()


def extract_text_isolated(data: bytes, max_chars: int, timeout: float = MAX_EXTRACT_SECONDS) -> Extracted:
    """`extract_text` in a process of its own, killed when it runs out of time.

    Size, pages and characters bound what a file may ask for, not what
    reading it costs: a small PDF can still be built to keep a parser busy
    for minutes or to unpack into gigabytes. A thread cannot be stopped; a
    process can, so the file is read in one, with a time limit (and, on
    Linux, a memory limit), and the server only waits for the answer."""
    if not data:
        raise UploadError(422, "the upload is empty")
    if len(data) > MAX_UPLOAD_BYTES:
        raise too_large()
    # spawn, not fork: the server has threads, and the child needs nothing of it but this module.
    context = multiprocessing.get_context("spawn")
    receiver, sender = context.Pipe(duplex=False)
    child = context.Process(target=_work, args=(sender, data, max_chars), daemon=True)
    child.start()
    sender.close()
    try:
        if not receiver.poll(timeout):
            raise UploadError(
                422, f"the file took more than {timeout:g} seconds to read; it is not an ordinary document"
            )
        try:
            answer = receiver.recv()
        except EOFError:  # the child died without answering: out of memory, or a crash in a parser
            raise UploadError(422, "the file could not be read: reading it used too much memory or failed") from None
    finally:
        receiver.close()
        if child.is_alive():
            child.kill()
        child.join()
    if answer[0] == "refused":
        raise UploadError(answer[1], answer[2])
    return Extracted(kind=answer[1], text=answer[2], pages=answer[3])


def _too_much_text(max_chars: int) -> UploadError:
    return UploadError(422, f"too much text: the limit is {max_chars:,} characters of extracted text")


def _clean(text: str) -> str:
    """Line endings as \\n, nothing unprintable, no trailing space, no blank lines at either end.

    Letters are written the way somebody typing them would: composed (NFC, so
    "é" is one character however the file spells it) and with ligatures spelled
    out. What a parser copies from this text is checked against it character
    by character (app.grounding), and a model retypes "Certified", not "Certiﬁed"."""
    text = _UNPRINTABLE.sub("", text.replace("\r\n", "\n").replace("\r", "\n"))
    text = _LIGATURES.sub(lambda m: unicodedata.normalize("NFKC", m.group()), unicodedata.normalize("NFC", text))
    return "\n".join(line.rstrip() for line in text.split("\n")).strip("\n")


def _pdf_text(data: bytes, max_chars: int) -> tuple[str, int]:
    import pypdf

    try:
        reader = pypdf.PdfReader(io.BytesIO(data))
        # A PDF that only restricts printing or copying is encrypted with an empty password and opens with it.
        if reader.is_encrypted and not _opens_without_password(reader):
            raise UploadError(422, "the PDF is password-protected; upload an unprotected copy")
        count = len(reader.pages)
        if count > MAX_PDF_PAGES:
            raise UploadError(422, f"too many pages: the limit is {MAX_PDF_PAGES}, this PDF has {count}")
        parts: list[str] = []
        total = 0
        for page in reader.pages:
            parts.append(page.extract_text())
            # Counted as it will be counted at the end, cleaned; the text as a whole is never shorter than its pages.
            total += len(_clean(parts[-1]))
            if total > max_chars:  # stop reading: the rest can only make it longer
                raise _too_much_text(max_chars)
    except UploadError:
        raise
    except Exception as e:  # pypdf raises many types on a damaged file; none of them is worth a 500
        raise UploadError(422, f"the PDF could not be read: {type(e).__name__}") from e
    return "\n".join(parts), count


def _opens_without_password(reader: Any) -> bool:
    try:
        return bool(reader.decrypt(""))
    except Exception:  # e.g. AES without the optional crypto package: it cannot be opened here
        return False


def _margins(names: list[str], which: str) -> list[str]:
    """The header (or footer) parts in the order Word numbers them: header2 before header10."""
    found = [n for n in names if _DOCX_MARGINS.match(n) and which in n]
    return sorted(found, key=lambda n: int("".join(c for c in n if c.isdigit()) or 0))


def _docx_text(archive: zipfile.ZipFile) -> str:
    try:
        names = archive.namelist()
        # Contact details often sit in the header, so the margins are read too.
        parts = [*_margins(names, "header"), _DOCX_BODY, *_margins(names, "footer")]
        budget = MAX_DOCX_XML_BYTES
        texts: list[str] = []
        for name in parts:
            xml = _read_member(archive, name, budget)
            budget -= len(xml)
            text = _paragraphs(xml)
            # A document with a different first page has the same header in several parts: once is enough.
            if text and text not in texts:
                texts.append(text)
    except UploadError:
        raise
    except Exception as e:  # zipfile, zlib and expat raise many types on a damaged file; none is worth a 500
        raise UploadError(422, f"the DOCX could not be read: {type(e).__name__}") from e
    return "\n".join(texts)


def _read_member(archive: zipfile.ZipFile, name: str, budget: int) -> bytes:
    """One member, read in chunks so a part that unpacks past the budget is abandoned, not held in memory."""
    chunks: list[bytes] = []
    size = 0
    with archive.open(name) as member:
        while chunk := member.read(_READ_CHUNK):
            size += len(chunk)
            if size > budget:
                raise UploadError(
                    422, f"the DOCX unpacks to more than {megabytes(MAX_DOCX_XML_BYTES)} of text; it is not a resume"
                )
            chunks.append(chunk)
    return b"".join(chunks)


class _NoDoctype(ElementTree.TreeBuilder):
    """Builds the tree, and refuses a document type declaration: a Word document declares no DTD and no
    entities, and one that does is an attack on the XML parser. Refused by the parser itself, as soon as the
    declaration ends, so it holds whatever encoding the part is written in."""

    def doctype(self, *_: object) -> None:
        raise UploadError(422, "the DOCX could not be read: it declares XML entities")


def _paragraphs(xml: bytes) -> str:
    """The text of one WordprocessingML part, a line per paragraph."""
    parser = ElementTree.XMLParser(target=_NoDoctype())
    parser.feed(xml)
    lines: list[str] = []
    _collect(parser.close(), [], lines)
    return "\n".join(lines)


def _collect(node: ElementTree.Element, runs: list[str], lines: list[str]) -> None:
    """Walk a part: text goes to `runs`, the paragraph being read, and every paragraph ends up a line of `lines`.

    A paragraph inside another (a text box, a sidebar) is its own line after
    the one that holds it, never part of it as well. Word writes a drawing
    twice, as the real thing and as a fallback for old readers; the fallback
    is skipped, or its text would be there twice."""
    for child in node:
        if child.tag == f"{_MC}Fallback":
            continue
        if child.tag == f"{_W}p":
            own: list[str] = []
            nested: list[str] = []
            _collect(child, own, nested)
            lines.append("".join(own))
            lines.extend(nested)
        elif child.tag == f"{_W}t":
            runs.append(child.text or "")
        elif child.tag == f"{_W}tab":
            runs.append("\t")
        elif child.tag in (f"{_W}br", f"{_W}cr"):
            runs.append("\n")
        else:
            _collect(child, runs, lines)
