#!/usr/bin/env python3
"""Build the Pages site: README.md becomes index.html, the other README and
every docs/*.md keep their path with a .html suffix, so repo-relative images
and cross-page links keep working unchanged. Everything under docs/ that is
not a page is copied verbatim; a .md link that points outside the converted
set is kept alive by copying the raw file in, and a link to any other repo
file goes to its GitHub blob, so nothing 404s.

Requires the `markdown` package (`python3 -m pip install markdown`).
"""

import argparse
import os
import re
import shutil
from pathlib import Path

import markdown

REPO = Path(__file__).resolve().parent.parent
BRAND = "asq"
REPO_URL = "https://github.com/itswl/agent-session-query"
# README.md <-> README.zh-CN.md: each carries a switch to the other.
LANG_SWITCH = {
    Path("README.md"): ("README.zh-CN.html", "中文"),
    Path("README.zh-CN.md"): ("index.html", "English"),
}
MD_EXTENSIONS = ["tables", "fenced_code"]
IMG_EXTS = {".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico"}

CSS = """
:root{--fg:#1f2328;--bg:#ffffff;--muted:#59636e;--border:#d1d9e0;--code:#f6f8fa;--link:#0969da;color-scheme:light dark}
@media(prefers-color-scheme:dark){:root{--fg:#f0f6fc;--bg:#0d1117;--muted:#9198a1;--border:#3d444d;--code:#161b22;--link:#4493f8}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans",Helvetica,Arial,sans-serif}
nav{max-width:840px;margin:0 auto;padding:1.1rem 1.25rem;display:flex;gap:1.2rem;align-items:baseline;border-bottom:1px solid var(--border)}
nav .brand{font-weight:650;color:var(--fg);text-decoration:none}
nav a{color:var(--link);text-decoration:none}
nav a:hover{text-decoration:underline}
main{max-width:840px;margin:0 auto;padding:1.5rem 1.25rem 4rem}
img,video{max-width:100%}
h1,h2,h3,h4{line-height:1.3;margin:1.8em 0 .6em}
h1{margin-top:.4em}
code,pre{font-family:ui-monospace,"SFMono-Regular",Menlo,Consolas,monospace;font-size:.92em}
code{background:var(--code);padding:.15em .4em;border-radius:6px}
pre{background:var(--code);padding:1rem;border-radius:8px;overflow-x:auto;line-height:1.55}
pre code{background:none;padding:0;font-size:1em}
table{border-collapse:collapse;margin:1em 0;display:block;overflow-x:auto}
th,td{border:1px solid var(--border);padding:.45em .8em}
th{background:var(--code)}
blockquote{border-left:3px solid var(--border);margin:1em 0;padding:.1em 1em;color:var(--muted)}
a{color:var(--link)}
hr{border:0;border-top:1px solid var(--border);margin:2.5rem 0}
"""

TEMPLATE = """<!doctype html>
<html lang="@@LANG@@">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>@@TITLE@@</title>
<style>@@CSS@@</style>
</head>
<body>
<nav>@@NAV@@</nav>
<main>
@@BODY@@
</main>
</body>
</html>
"""


def sources() -> list[Path]:
    """The pages the site carries: both READMEs and every docs/*.md."""
    pages = [REPO / "README.md"]
    zh = REPO / "README.zh-CN.md"
    if zh.is_file():
        pages.append(zh)
    docs = REPO / "docs"
    if docs.is_dir():
        pages += sorted(docs.glob("*.md"))
    return [p for p in pages if p.is_file()]


def out_rel(rel: Path) -> Path:
    """Where a converted page lands: README.md is the site's index."""
    if rel.name == "README.md" and rel.parent == Path("."):
        return Path("index.html")
    return rel.with_suffix(".html")


def rel_to_root(rel: Path, target: str) -> str:
    """A repo-root-relative path, rewritten to be right from rel's directory."""
    return os.path.relpath(target, rel.parent.as_posix() or ".")


def convert(rel: Path, text: str, html_set: set[Path]) -> tuple[str, list[Path]]:
    """Markdown to HTML, with relative links pointed where they resolve.

    A link to a converted page goes to that page; to any other .md, it is
    left and the target reported so the raw file ships as a fallback; to
    any other existing repo file (source, scripts), to its GitHub blob;
    images are left alone, because docs/ ships with the site. Everything
    else — external URLs, anchors, dangling targets — passes through.
    """
    body = markdown.markdown(text, extensions=MD_EXTENSIONS)
    raw: list[Path] = []

    def link(m: re.Match) -> str:
        attr, target, anchor = m.group(1), m.group(2), m.group(3) or ""
        if target.startswith(("http://", "https://", "#", "mailto:")):
            return m.group(0)
        dest = (REPO / rel.parent / target).resolve()
        try:
            t = dest.relative_to(REPO)
        except ValueError:
            return m.group(0)
        if t in html_set:
            href = rel_to_root(rel, out_rel(t).as_posix())
            return f'{attr}="{href}{anchor}"'
        if t.suffix == ".md":
            raw.append(t)
            return m.group(0)
        if t.suffix.lower() in IMG_EXTS:
            return m.group(0)
        if (REPO / t).is_file():
            return f'{attr}="{REPO_URL}/blob/main/{t.as_posix()}{anchor}"'
        return m.group(0)

    body = re.sub(r'(href|src)="([^"#]+)(#[^"]*)?"', link, body)
    return body, raw


def title_of(body: str, fallback: str) -> str:
    m = re.search(r"<h1[^>]*>(.*?)</h1>", body, re.S)
    if not m:
        return fallback
    return re.sub(r"<[^>]+>", "", m.group(1)).strip()


def render(rel: Path, body: str) -> str:
    nav = f'<a class="brand" href="{rel_to_root(rel, "index.html")}">{BRAND}</a>'
    switch = LANG_SWITCH.get(rel)
    if switch:
        nav += f'<a href="{rel_to_root(rel, switch[0])}">{switch[1]}</a>'
    nav += f'<a href="{REPO_URL}">GitHub</a>'
    page = TEMPLATE
    for token, value in {
        "@@CSS@@": CSS,
        "@@NAV@@": nav,
        "@@TITLE@@": title_of(body, f"{BRAND} — {rel.name}"),
        "@@BODY@@": body,
        "@@LANG@@": "zh" if "zh-CN" in rel.name else "en",
    }.items():
        page = page.replace(token, value)
    return page


def copy_assets(out: Path, html_set: set[Path]) -> None:
    """Everything under docs/ that is not a page: screenshots and the like."""
    docs = REPO / "docs"
    if not docs.is_dir():
        return
    for p in docs.rglob("*"):
        if p.is_file() and p.relative_to(REPO) not in html_set:
            d = out / p.relative_to(REPO)
            d.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(p, d)


def copy_raw(out: Path, wanted: set[Path], html_set: set[Path]) -> None:
    """Non-page .md files some link points at, copied as-is so nothing 404s."""
    for rel in sorted(wanted):
        if rel in html_set:
            continue
        src = REPO / rel
        if not src.is_file():
            continue
        d = out / rel
        d.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, d)


def main() -> None:
    ap = argparse.ArgumentParser(description="build the Pages site from the READMEs and docs/")
    ap.add_argument("--out", required=True, help="output directory for the site")
    args = ap.parse_args()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)

    rels = [p.relative_to(REPO) for p in sources()]
    html_set = set(rels)
    raw_needed: set[Path] = set()

    for rel in rels:
        text = (REPO / rel).read_text(encoding="utf-8")
        body, raw = convert(rel, text, html_set)
        raw_needed.update(raw)
        dest = out / out_rel(rel)
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(render(rel, body), encoding="utf-8")

    copy_assets(out, html_set)
    copy_raw(out, raw_needed, html_set)
    print(f"{len(rels)} pages -> {out} ({len(raw_needed)} raw md copies)")


if __name__ == "__main__":
    main()
