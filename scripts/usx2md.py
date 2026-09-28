#!/usr/bin/env python3
"""Convert a USX 3.x Bible into one markdown file per book.

Deliberately NOT a Source plugin. A Source exists for content that changes:
List, Fetch and an updated_at are machinery for incremental reconciliation, and
scripture is static. This runs once and the ordinary filesystem source indexes
the result.

The output is shaped for the chunker rather than for reading. Section headings
(USX style s1/s2) become `##`, which is what makes chunks come out at PERICOPE
granularity: a complete thought of a few hundred words. Verse-level chunks would
be ~31,000 fragments averaging 25 words, each stripped of the context that gave
it meaning, which is the single worst thing you can do to an embedding index.

Footnotes and cross-reference apparatus are dropped. They are editorial
machinery, not scripture, and they embed as noise inside the passage they
annotate.

    python3 scripts/usx2md.py ~/Downloads/bsb_usx <out-dir>
"""
import re
import sys
import pathlib
import xml.etree.ElementTree as ET

# Paragraph styles that carry scripture text. Everything else (running heads,
# tables of contents, title pages) is front matter.
BODY = re.compile(r"^(p|m|pi\d?|q\d?|li\d?|pc|mi|nb|b|cls|pmo|pm|pmc|pmr|ph\d?)$")
HEAD = re.compile(r"^(s\d?|ms\d?)$")
SKIP_TAGS = {"note", "ref"}          # apparatus: footnotes and cross-references


def text_of(el):
    """Visible text of an element, minus apparatus, with verse markers kept."""
    out = []
    if el.tag == "verse" and el.get("number"):
        out.append(f" **{el.get('number')}** ")
    if el.text:
        out.append(el.text)
    for child in el:
        if child.tag not in SKIP_TAGS:
            out.append(text_of(child))
        # A skipped element's tail is still scripture, so it is never dropped.
        if child.tail:
            out.append(child.tail)
    return "".join(out)


def convert(path):
    root = ET.parse(path).getroot()
    name, chapter = path.stem, None
    sections = []                     # [(heading, [paragraph, ...])]
    cur = None

    for el in root:
        style = el.get("style", "")
        # Prefer the running head (h): "Matthew", not toc1's "The Gospel
        # According to Matthew". The book name is prefixed onto every chunk of
        # the book before embedding, so a long one dilutes all of them.
        if el.tag == "para" and style == "h" and (el.text or "").strip():
            name = el.text.strip()
        elif el.tag == "chapter" and el.get("number"):
            chapter = el.get("number")
        elif el.tag == "para" and HEAD.match(style):
            cur = ((el.text or "").strip(), chapter, [])
            sections.append(cur)
        elif el.tag == "para" and BODY.match(style):
            body = re.sub(r"[ \t]+", " ", text_of(el)).strip()
            if not body:
                continue
            if cur is None:           # text before any heading
                cur = ("", chapter, [])
                sections.append(cur)
            cur[2].append(body)

    lines = [f"# {name}", ""]
    for heading, ch, paras in sections:
        if not paras:
            continue
        label = f"{name} {ch}" if ch else name
        lines.append(f"## {label}" + (f" — {heading}" if heading else ""))
        lines.append("")
        lines += ["\n".join(paras), ""]
    return name, "\n".join(lines)


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    src, dst = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    dst.mkdir(parents=True, exist_ok=True)
    files = sorted(src.rglob("*.usx"))
    if not files:
        sys.exit(f"no .usx files under {src}")

    total = 0
    for i, f in enumerate(files, 1):
        name, md = convert(f)
        # Numbered so canonical order survives an alphabetical file listing.
        out = dst / f"{i:02d} {name}.md"
        out.write_text(md)
        total += md.count("\n## ")
    print(f"{len(files)} books -> {dst}  ({total} sections)")


if __name__ == "__main__":
    main()
