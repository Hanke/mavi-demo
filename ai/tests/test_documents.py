"""/extract-text: a PDF or a DOCX comes back as its text, and anything else,
anything too big and anything unreadable is refused with a status and a
sentence that says why."""

from __future__ import annotations

import io
import multiprocessing
import zipfile
from xml.sax.saxutils import escape

import pytest
from fastapi.testclient import TestClient

from app import documents, fixtures, grounding
from app.main import MAX_TEXT_CHARS, app
from app.settings import Settings, get_settings

app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)
client = TestClient(app)

_W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"


def _part(paragraphs: list[str], root: str = "document") -> str:
    body = "".join(f"<w:p><w:r><w:t>{escape(p)}</w:t></w:r></w:p>" for p in paragraphs)
    inner = f"<w:body>{body}</w:body>" if root == "document" else body
    return f'<?xml version="1.0" encoding="UTF-8"?><w:{root} xmlns:w="{_W}">{inner}</w:{root}>'


def _docx(paragraphs: list[str], extra: dict[str, str] | None = None) -> bytes:
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("[Content_Types].xml", "<Types/>")
        archive.writestr("word/document.xml", _part(paragraphs))
        for name, content in (extra or {}).items():
            archive.writestr(name, content)
    return out.getvalue()


def _post(data: bytes, content_type: str = "application/octet-stream"):
    return client.post("/extract-text", content=data, headers={"Content-Type": content_type})


# --- what is accepted --------------------------------------------------------


@pytest.mark.parametrize("slug", fixtures.resume_slugs())
def test_a_pdf_comes_back_as_the_text_it_was_rendered_from(slug: str):
    fixture = fixtures.load_resume(slug)
    resp = _post(fixture.pdf_path.read_bytes(), "application/pdf")
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["kind"] == "pdf"
    assert body["pages"] >= 1
    # The PDF wraps long lines, so the words are compared, not the line breaks.
    assert body["text"].split() == fixture.text.split()


def test_a_docx_comes_back_as_its_paragraphs_with_header_and_footer():
    data = _docx(
        ["Dana Whitfield", "Bookkeeper, QuickBooks & Xero", ""],
        {
            "word/header1.xml": _part(["dana@example.com"], "hdr"),
            "word/footer1.xml": _part(["Page 1"], "ftr"),
        },
    )
    resp = _post(data)
    assert resp.status_code == 200, resp.text
    assert resp.json() == {
        "text": "dana@example.com\nDana Whitfield\nBookkeeper, QuickBooks & Xero\n\nPage 1",
        "kind": "docx",
        "pages": None,
    }


def test_docx_tabs_and_line_breaks_are_kept():
    xml = (
        f'<w:document xmlns:w="{_W}"><w:body><w:p><w:r><w:t>2019</w:t><w:tab/><w:t>Controller</w:t>'
        "<w:br/><w:t>Acme</w:t></w:r></w:p></w:body></w:document>"
    )
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("word/document.xml", xml)
    assert _post(out.getvalue()).json()["text"] == "2019\tController\nAcme"


def test_a_text_box_is_a_line_of_its_own_and_is_there_once():
    w, mc = f'xmlns:w="{_W}"', 'xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"'
    box = "<w:txbxContent><w:p><w:r><w:t>Sidebar skills</w:t></w:r></w:p></w:txbxContent>"
    xml = (
        f"<w:document {w} {mc}><w:body><w:p><w:r><w:t>Outer</w:t></w:r><w:r><mc:AlternateContent>"
        f"<mc:Choice>{box}</mc:Choice><mc:Fallback>{box}</mc:Fallback></mc:AlternateContent></w:r></w:p>"
        "<w:p><w:r><w:t>Next</w:t></w:r></w:p></w:body></w:document>"
    )
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("word/document.xml", xml)
    assert _post(out.getvalue()).json()["text"] == "Outer\nSidebar skills\nNext"


def test_a_header_repeated_for_the_first_page_is_read_once_and_in_order():
    extra = {f"word/header{n}.xml": _part([text], "hdr") for n, text in ((10, "Page two"), (2, "dana@example.com"))}
    extra["word/header1.xml"] = _part(["dana@example.com"], "hdr")
    assert _post(_docx(["Dana Whitfield"], extra)).json()["text"] == "dana@example.com\nPage two\nDana Whitfield"


def test_a_docx_with_pdf_magic_in_its_first_bytes_is_still_a_docx():
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("docProps/%PDF-notes.txt", "x")
        archive.writestr("word/document.xml", _part(["Dana Whitfield"]))
    assert _post(out.getvalue()).json() == {"text": "Dana Whitfield", "kind": "docx", "pages": None}


def test_a_pdf_that_only_restricts_printing_opens_without_a_password():
    import pypdf

    fixture = fixtures.load_resume("bookkeeper_part_time")
    writer = pypdf.PdfWriter(clone_from=io.BytesIO(fixture.pdf_path.read_bytes()))
    writer.encrypt(user_password="", owner_password="owner", algorithm="RC4-128")
    out = io.BytesIO()
    writer.write(out)
    resp = _post(out.getvalue())
    assert resp.status_code == 200, resp.text
    assert resp.json()["text"].split() == fixture.text.split()


def test_the_pdf_text_limit_counts_the_text_as_returned(monkeypatch: pytest.MonkeyPatch):
    """Padding the cleaned text does not keep is not held against the file."""
    import pypdf

    class Page:
        def extract_text(self) -> str:
            return "\n".join("word" + " " * 40 for _ in range(10))

    class Reader:
        is_encrypted = False
        pages = (Page(), Page())

        def __init__(self, stream: io.BytesIO):
            pass

    monkeypatch.setattr(pypdf, "PdfReader", Reader)
    out = documents.extract_text(b"%PDF-1.4", max_chars=100)  # 880 characters as extracted, 99 once cleaned
    assert len(out.text) == 99
    with pytest.raises(documents.UploadError, match="too much text"):
        documents.extract_text(b"%PDF-1.4", max_chars=98)


def test_a_file_is_read_in_a_process_that_is_stopped_when_it_takes_too_long():
    pdf = fixtures.load_resume("controller_manufacturing").pdf_path.read_bytes()
    # In its own process the answer is the same, and so is a refusal.
    assert documents.extract_text_isolated(pdf, MAX_TEXT_CHARS) == documents.extract_text(pdf, MAX_TEXT_CHARS)
    with pytest.raises(documents.UploadError, match="too many pages") as refused:
        documents.extract_text_isolated(fixtures.render_pdf("\n".join("line" for _ in range(900))), MAX_TEXT_CHARS)
    assert refused.value.status == 422
    # No file is read in a millisecond: the limit is what answers, and the reader is not left running.
    with pytest.raises(documents.UploadError, match=r"took more than 0\.001 seconds") as slow:
        documents.extract_text_isolated(pdf, MAX_TEXT_CHARS, timeout=0.001)
    assert slow.value.status == 422
    assert multiprocessing.active_children() == []


def test_the_type_is_read_from_the_content_not_the_label():
    pdf = fixtures.load_resume("bookkeeper_part_time").pdf_path.read_bytes()
    # A PDF sent as "an image" is still a PDF; a text file sent as "a PDF" still is not one.
    assert _post(pdf, "image/png").json()["kind"] == "pdf"
    resp = _post(b"Dana Whitfield\nBookkeeper\n", "application/pdf")
    assert resp.status_code == 415


def test_control_characters_and_invisible_tag_letters_are_stripped():
    data = _docx(["Staff Accountant\U000e0041\U000e0042", "CPA\x85"])
    assert _post(data).json()["text"] == "Staff Accountant\nCPA"


# --- what is refused ---------------------------------------------------------


@pytest.mark.parametrize(
    ("data", "said"),
    [
        (b"Dana Whitfield\nBookkeeper\n", "only PDF and DOCX"),
        (b"\x89PNG\r\n\x1a\n" + b"\0" * 64, "only PDF and DOCX"),
        (b"<html><body>resume</body></html>", "only PDF and DOCX"),
        (b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" + b"\0" * 64, "legacy Word (.doc)"),
    ],
    ids=["text", "png", "html", "doc"],
)
def test_a_file_that_is_not_a_pdf_or_a_docx_is_unsupported(data: bytes, said: str):
    resp = _post(data)
    assert resp.status_code == 415
    assert resp.json()["detail"].startswith("unsupported file type")
    assert said in resp.json()["detail"]


def test_a_zip_that_is_not_a_word_document_is_unsupported():
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("xl/workbook.xml", "<workbook/>")
    assert _post(out.getvalue()).status_code == 415


def test_an_oversized_file_is_refused_before_it_is_read():
    limit = documents.MAX_UPLOAD_BYTES
    pdf = fixtures.load_resume("bookkeeper_part_time").pdf_path.read_bytes()
    resp = _post(pdf + b"\n" * (limit + 1 - len(pdf)))
    assert resp.status_code == 413
    assert resp.json()["detail"] == "file too large: the limit is 5.0 MB"
    # Right at the limit it is read.
    assert _post(pdf + b"\n" * (limit - len(pdf))).status_code == 200


def test_the_size_limit_does_not_depend_on_the_declared_length():
    """A body sent in chunks declares no length; it is cut off at the limit all the same."""

    def chunks():
        for _ in range(documents.MAX_UPLOAD_BYTES // 65536 + 2):
            yield b"%PDF-1.4" + b"\0" * 65528

    resp = client.post("/extract-text", content=chunks())
    assert resp.status_code == 413
    with pytest.raises(documents.UploadError, match="file too large") as caught:
        documents.extract_text(b"%PDF-" + b"\0" * documents.MAX_UPLOAD_BYTES, MAX_TEXT_CHARS)
    assert caught.value.status == 413


def test_a_pdf_with_too_many_pages_is_refused():
    lines_per_page = 60
    many = fixtures.render_pdf("\n".join(f"line {i}" for i in range(lines_per_page * (documents.MAX_PDF_PAGES + 2))))
    resp = _post(many)
    assert resp.status_code == 422
    detail = resp.json()["detail"]
    assert detail.startswith(f"too many pages: the limit is {documents.MAX_PDF_PAGES}, this PDF has ")
    assert int(detail.rsplit(" ", 1)[1]) > documents.MAX_PDF_PAGES


def test_too_much_text_is_refused_not_truncated():
    paragraph = "month-end close " * 100
    data = _docx([paragraph] * (MAX_TEXT_CHARS // len(paragraph) + 1))
    resp = _post(data)
    assert resp.status_code == 422
    assert resp.json()["detail"] == "too much text: the limit is 60,000 characters of extracted text"
    with pytest.raises(documents.UploadError, match="too much text"):
        documents.extract_text(fixtures.render_pdf("word " * 400), 1000)


def test_a_docx_that_unpacks_to_far_more_than_it_weighs_is_refused():
    bomb = _docx(["x" * (documents.MAX_DOCX_XML_BYTES + 1)])
    assert len(bomb) < documents.MAX_UPLOAD_BYTES  # it compresses to a few kilobytes
    resp = _post(bomb)
    assert resp.status_code == 422
    assert "unpacks to more than" in resp.json()["detail"]


def test_a_docx_that_declares_xml_entities_is_refused():
    xml = (
        '<?xml version="1.0"?><!DOCTYPE d [<!ENTITY a "aaaa"><!ENTITY b "&a;&a;&a;&a;">]>'
        f'<w:document xmlns:w="{_W}"><w:body><w:p><w:r><w:t>&b;</w:t></w:r></w:p></w:body></w:document>'
    )
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("word/document.xml", xml)
    resp = _post(out.getvalue())
    assert resp.status_code == 422
    assert "XML entities" in resp.json()["detail"]
    # In any encoding: the parser refuses the declaration, not a search of the bytes.
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("word/document.xml", xml.replace('<?xml version="1.0"?>', "").encode("utf-16"))
    resp = _post(out.getvalue())
    assert resp.status_code == 422
    assert "XML entities" in resp.json()["detail"]


@pytest.mark.parametrize(
    ("data", "said"),
    [
        (b"", "the upload is empty"),
        (b"%PDF-1.4\nnot really a pdf", "the PDF could not be read"),
        (fixtures.render_pdf(""), "no text could be extracted"),
        (_docx([]), "no text could be extracted"),
    ],
    ids=["empty", "damaged-pdf", "blank-pdf", "blank-docx"],
)
def test_a_file_with_nothing_to_read_is_a_clear_error(data: bytes, said: str):
    resp = _post(data)
    assert resp.status_code == 422
    assert said in resp.json()["detail"]


def test_a_damaged_docx_is_refused_not_a_server_error():
    whole = _docx(["Dana Whitfield"])
    # Cut short, it is no longer a zip anyone can open, so it is not a DOCX at all.
    assert _post(whole[: len(whole) // 2]).status_code == 415
    # With a body that is not XML, it is a DOCX that cannot be read.
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        archive.writestr("word/document.xml", "<w:document")
    resp = _post(out.getvalue())
    assert resp.status_code == 422
    assert resp.json()["detail"] == "the DOCX could not be read: ParseError"
    # With a compressed body that does not inflate, the same.
    whole = _docx(["month-end close " * 200])
    at = whole.index(b"word/document.xml") + len("word/document.xml") + 8
    resp = _post(whole[:at] + bytes(b ^ 0xFF for b in whole[at : at + 16]) + whole[at + 16 :])
    assert resp.status_code == 422
    assert resp.json()["detail"].startswith("the DOCX could not be read: ")


def test_a_password_protected_pdf_is_refused():
    import pypdf

    writer = pypdf.PdfWriter(clone_from=io.BytesIO(fixtures.load_resume("bookkeeper_part_time").pdf_path.read_bytes()))
    writer.encrypt("secret", algorithm="RC4-128")
    out = io.BytesIO()
    writer.write(out)
    resp = _post(out.getvalue())
    assert resp.status_code == 422
    assert "password-protected" in resp.json()["detail"]


def test_the_extracted_text_always_fits_the_parsers():
    """Whatever /extract-text returns can be sent to /parse-resume: the same limit bounds both."""
    schema = app.openapi()["components"]["schemas"]["ParseResumeRequest"]["properties"]["text"]
    assert schema["maxLength"] == MAX_TEXT_CHARS


def test_text_is_written_the_way_a_reader_would_type_it():
    """What a parser copies is checked against this text character by
    character, so what a PDF hides between letters, and its ligatures, would
    make a plainly typed quote miss."""
    data = _docx(
        ["Certiﬁed Public Accountant", "co\xadordinated the month-end close", "Senior\u200b Accountant, Café Group"]
    )
    text = _post(data).json()["text"]
    assert text == "Certified Public Accountant\ncoordinated the month-end close\nSenior Accountant, Café Group"
    assert grounding.find_quote("Certified Public Accountant", text) == "Certified Public Accountant"
    # A lone surrogate is not text and would fail the response's encoding.
    assert documents._clean("CPA \ud800licence") == "CPA licence"  # pyright: ignore[reportPrivateUsage]


def test_a_quote_matches_whatever_hyphen_the_page_uses():
    for hyphen in map(chr, (0x2010, 0x2011, 0x2212, 0x2013)):
        page = f"Led the month{hyphen}end close"
        assert grounding.find_quote("month-end close", page) == f"month{hyphen}end close"
        assert grounding.in_text("month-end", page)
