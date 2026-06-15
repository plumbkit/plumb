#!/usr/bin/env python3
"""
build-blog.py — render the plumb blog from Markdown posts into styled HTML.

Pipeline:

    site/blog/posts/YYYY-MM-DD-slug.md  --(frontmatter + Markdown)-->
        site/blog/<slug>.html   (one styled page per post)
        site/blog/index.html    (the blog index, newest first)
        site/blog/pygments.css  (syntax-highlight theme, light + dark)

Markdown is the source of truth. The generated *.html and pygments.css are built
here (and git-ignored — see site/blog/.gitignore); blog.css and authors.toml are
committed. Posts are written by plumb's AI authors (see the private authoring
workspace); each post's `author` keys into site/blog/authors.toml for the public
byline. Styling reuses the site palette via site/blog/blog.css.

Requirements (pip): markdown, pygments — see scripts/requirements.txt.
TOML parsing uses the stdlib `tomllib` (Python 3.11+).

Usage:
    python3 scripts/build-blog.py                     # site/blog/ in place
    python3 scripts/build-blog.py --site-dir site     # explicit site root
"""

from __future__ import annotations

import argparse
import html
import sys
import tomllib
from pathlib import Path

import markdown
from pygments.formatters import HtmlFormatter

REQUIRED_KEYS = ("title", "author", "date", "description")
LIGHT_PYGMENTS = "friendly"
DARK_PYGMENTS = "monokai"


def die(msg: str) -> None:
    sys.exit(f"build-blog: {msg}")


# ---- frontmatter ---------------------------------------------------------------


def split_frontmatter(text: str, path: Path) -> tuple[dict, str]:
    """Split a `---`-delimited frontmatter block from the Markdown body."""
    if not text.startswith("---"):
        die(f"{path}: missing '---' frontmatter block at the top of the file")
    parts = text.split("\n---", 1)
    if len(parts) != 2:
        die(f"{path}: frontmatter block is not closed with a '---' line")
    raw = parts[0][len("---"):].strip("\n")
    body = parts[1].lstrip("\n")
    return parse_frontmatter(raw, path), body


def parse_frontmatter(raw: str, path: Path) -> dict:
    """Parse the small YAML subset our posts use: scalars, booleans, inline lists."""
    meta: dict = {}
    for line in raw.splitlines():
        line = line.rstrip()
        if not line or line.lstrip().startswith("#"):
            continue
        if ":" not in line:
            die(f"{path}: malformed frontmatter line (no ':'): {line!r}")
        key, _, value = line.partition(":")
        meta[key.strip()] = parse_scalar(value.strip())
    return meta


def parse_scalar(value: str):
    if value.startswith("[") and value.endswith("]"):
        inner = value[1:-1].strip()
        if not inner:
            return []
        return [parse_scalar(v.strip()) for v in inner.split(",")]
    if len(value) >= 2 and value[0] in "\"'" and value[-1] == value[0]:
        return value[1:-1]
    low = value.lower()
    if low in ("true", "false"):
        return low == "true"
    return value


# ---- model ---------------------------------------------------------------------


def slug_for(path: Path) -> str:
    """`2026-06-15-why-lsp.md` -> `why-lsp`; falls back to the bare stem."""
    stem = path.stem
    parts = stem.split("-", 3)
    if len(parts) == 4 and parts[0].isdigit():
        return parts[3]
    return stem


def load_authors(path: Path) -> dict:
    if not path.exists():
        die(f"missing authors file: {path}")
    with path.open("rb") as f:
        data = tomllib.load(f)
    return data.get("authors", data)


def load_posts(posts_dir: Path, authors: dict) -> list[dict]:
    posts = []
    for md_path in sorted(posts_dir.glob("*.md")):
        meta, body = split_frontmatter(md_path.read_text(encoding="utf-8"), md_path)
        for key in REQUIRED_KEYS:
            if not meta.get(key):
                die(f"{md_path}: missing required frontmatter key '{key}'")
        if meta.get("draft") is True:
            print(f"build-blog: skipping draft {md_path.name}")
            continue
        author_key = meta["author"]
        if author_key not in authors:
            die(f"{md_path}: unknown author '{author_key}' — add it to authors.toml")
        meta["slug"] = slug_for(md_path)
        meta["body"] = body
        meta["author_info"] = authors[author_key]
        posts.append(meta)
    posts.sort(key=lambda m: str(m["date"]), reverse=True)
    return posts


# ---- rendering -----------------------------------------------------------------


def make_md() -> markdown.Markdown:
    return markdown.Markdown(
        extensions=["fenced_code", "codehilite", "tables", "toc", "sane_lists", "smarty"],
        extension_configs={"codehilite": {"guess_lang": False, "css_class": "codehilite"}},
    )


def render_body(md: markdown.Markdown, body: str) -> str:
    md.reset()
    return md.convert(body)


def pygments_css() -> str:
    light = HtmlFormatter(style=LIGHT_PYGMENTS).get_style_defs(".codehilite")
    dark = HtmlFormatter(style=DARK_PYGMENTS).get_style_defs(".codehilite")
    dark_scoped = "\n".join(
        line if not line.strip() or line.startswith("/*") else f'[data-theme="dark"] {line}'
        for line in dark.splitlines()
    )
    dark_media = "\n".join(
        line if not line.strip() or line.startswith("/*")
        else f':root:not([data-theme="light"]) {line}'
        for line in dark.splitlines()
    )
    return (
        f"/* light (default) — pygments '{LIGHT_PYGMENTS}' */\n{light}\n\n"
        f'/* dark via explicit toggle — pygments {DARK_PYGMENTS!r} */\n{dark_scoped}\n\n'
        f"@media(prefers-color-scheme:dark){{\n{dark_media}\n}}\n"
    )


HEAD_THEME = (
    "<script>(function(){try{var t=localStorage.getItem('plumb-theme');"
    "if(t)document.documentElement.setAttribute('data-theme',t);}catch(e){}})();</script>"
)
# Header scripts, mirroring the homepage: glass-nav blur toggles on scroll, and
# the theme toggle persists to the same 'plumb-theme' localStorage key.
TOGGLE_SCRIPT = (
    "<script>"
    "(function(){var h=document.getElementById('hdr');if(h)addEventListener('scroll',"
    "function(){h.classList.toggle('scrolled',scrollY>24);},{passive:true});})();"
    "(function(){var b=document.getElementById('themeToggle');if(!b)return;"
    "b.addEventListener('click',function(){"
    "var cur=document.documentElement.getAttribute('data-theme')||"
    "(matchMedia('(prefers-color-scheme:dark)').matches?'dark':'light');"
    "var next=cur==='dark'?'light':'dark';"
    "document.documentElement.setAttribute('data-theme',next);"
    "try{localStorage.setItem('plumb-theme',next);}catch(e){}});})();</script>"
)
# Theme-toggle button + GitHub pill + logo wordmark — copied verbatim from
# site/index.html so the blog header is pixel-identical to the homepage.
THEME_BTN = (
    '<button class="thm" id="themeToggle" type="button" aria-label="Toggle colour theme">'
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" '
    'aria-hidden="true"><circle cx="12" cy="12" r="8.5"/>'
    '<path d="M12 3.5 A8.5 8.5 0 0 0 12 20.5 Z" fill="currentColor" stroke="none"/></svg></button>'
)
GH_LINK = (
    '<a class="gh" href="https://github.com/plumbkit/plumb">GitHub '
    '<svg class="ext" viewBox="0 0 16 16" fill="none" stroke="currentColor" '
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">'
    '<path d="M5.25 3.75h7v7"/><path d="M12.25 3.75 4 12"/></svg></a>'
)
LOGO_SVG = '<svg class="wm" viewBox="0 0 5150 2320" role="img" aria-label="plumb" fill="currentColor"><defs><clipPath id="wbc" clipPathUnits="userSpaceOnUse"><rect x="-300" y="-336.5" width="2200" height="2936.5"/></clipPath></defs><g transform="translate(0,1560) scale(1,-1)"><g clip-path="url(#wbc)"><g transform="translate(0 873) scale(1 -1)"><path d="M303 1467V76.5L119 -6Q115.5 -8 111.75 -9.5Q108 -11 104 -11Q100.5 -11 97.75 -8.5Q95 -6 95 -2V1375Q95 1380.5 92.25 1383.75Q89.5 1387 82.5 1387H11Q7 1387 5.0 1389.0Q3 1391 3 1393.5Q3 1397 5.25 1398.5Q7.5 1400 11 1401L274.5 1474.5Q283 1477 286.25 1477.5Q289.5 1478 293 1478Q298 1478 300.5 1474.75Q303 1471.5 303 1467ZM288 721 278 732Q354 821.5 422.5 857.25Q491 893 574 893Q680 893 762.75 840.5Q845.5 788 892.75 691.0Q940 594 940 460.5Q940 316 887.5 207.75Q835 99.5 742.0 39.75Q649 -20 528 -20Q428 -20 362.0 20.75Q296 61.5 255 131L272 139Q302.5 79 354.75 43.5Q407 8 478 8Q547 8 603.75 52.0Q660.5 96 694.0 191.5Q727.5 287 727.5 440.5Q727.5 580 698.75 670.75Q670 761.5 619.75 806.25Q569.5 851 504 851Q453.5 851 397.5 820.25Q341.5 789.5 288 721Z"/></g></g><path d="M201.00 -220.00 L381.00 -421.60 L201.00 -700.00 L21.00 -421.60 Z"/><g transform="translate(1028,0)"><path d="M348 1467V53Q348 40.5 353.75 33.0Q359.5 25.5 372 23L431 13.5Q437 13 439.0 11.0Q441 9 441 6Q441 3.5 439.0 1.75Q437 0 433 0H52Q49.5 0 47.25 1.75Q45 3.5 45 6Q45 8.5 48.0 11.0Q51 13.5 58 14.5L116 23Q129 25.5 134.5 33.0Q140 40.5 140 52V1375Q140 1380.5 137.25 1383.75Q134.5 1387 127.5 1387H56Q52 1387 50.0 1389.0Q48 1391 48 1393.5Q48 1397 50.25 1398.5Q52.5 1400 56 1401L319.5 1474.5Q328 1477 331.25 1477.5Q334.5 1478 338 1478Q343 1478 345.5 1474.75Q348 1471.5 348 1467Z"/></g><g transform="translate(1509,0)"><path d="M689.5 31V158V163V785Q689.5 790.5 686.75 793.75Q684 797 677 797H605.5Q601.5 797 599.5 799.0Q597.5 801 597.5 803.5Q597.5 807 599.75 808.5Q602 810 605.5 811L869 884.5Q877 887 880.5 887.5Q884 888 887.5 888Q892.5 888 895.0 884.75Q897.5 881.5 897.5 877V53Q897.5 40.5 903.25 33.0Q909 25.5 921.5 23L980.5 13.5Q986.5 13 988.5 11.0Q990.5 9 990.5 6Q990.5 3.5 988.5 1.75Q986.5 0 982.5 0H716.5Q703.5 0 696.5 7.75Q689.5 15.5 689.5 31ZM125 238V785Q125 790.5 122.0 793.75Q119 797 111.5 797H40Q36 797 34.0 799.0Q32 801 32 803.5Q32 807 34.25 808.5Q36.5 810 40 811L304.5 884.5Q313 887 316.25 887.5Q319.5 888 323 888Q328 888 330.5 884.75Q333 881.5 333 877V259Q333 164 376.5 120.0Q420 76 492 76Q537 76 583.5 95.5Q630 115 677 158L711 188.5L720 179L685 147.5Q583.5 56.5 504.0 20.25Q424.5 -16 354.5 -16Q255 -16 190.0 45.75Q125 107.5 125 238Z"/></g><g transform="translate(2534,0)"><path d="M338 878V52Q338 41 344.5 32.75Q351 24.5 363 22L422 13Q431 12 431 6Q431 0 422 0H43Q39.5 0 37.25 2.0Q35 4 35 6Q35 8 37.75 10.0Q40.5 12 45 13L106 23Q119 25.5 124.5 32.5Q130 39.5 130 49V785Q130 791 127.5 795.0Q125 799 118 799H44Q41.5 799 39.25 801.0Q37 803 37 805Q37 808 39.25 810.0Q41.5 812 46 813L310 887Q317 889.5 320.75 889.75Q324.5 890 328 890Q333.5 890 335.75 886.25Q338 882.5 338 878ZM320 688 311 696 345 728Q445 822 524.25 857.5Q603.5 893 673 893Q772.5 893 836.25 831.25Q900 769.5 900 639V53Q900 39 907.5 30.75Q915 22.5 929 20L973 13Q982 11 982 6Q982 0 971 0H607Q599 0 599 6Q599 12 609 13L664 21Q683 25 689.0 36.75Q695 48.5 695 64V617Q695 708 650.5 754.5Q606 801 536 801Q488 801 443.75 782.0Q399.5 763 353 719ZM875.5 688 866.5 696 897.5 731Q976 817.5 1044.25 855.25Q1112.5 893 1185.5 893Q1278 893 1336.75 831.25Q1395.5 769.5 1412.5 638L1489 55Q1491 39.5 1498.5 30.5Q1506 21.5 1520 19L1563 12Q1567 11.5 1569.0 10.0Q1571 8.5 1571 6Q1571 3.5 1569.0 1.75Q1567 0 1563 0H1187Q1179 0 1179 6Q1179 11 1189 13L1244 21Q1263 24.5 1270.0 37.0Q1277 49.5 1275 66L1207.5 617Q1195.5 708 1159.75 754.5Q1124 801 1059.5 801Q1020 801 983.0 781.5Q946 762 905.5 722Z"/></g><g transform="translate(4112,0)"><path d="M303 1467V76.5L119 -6Q115.5 -8 111.75 -9.5Q108 -11 104 -11Q100.5 -11 97.75 -8.5Q95 -6 95 -2V1375Q95 1380.5 92.25 1383.75Q89.5 1387 82.5 1387H11Q7 1387 5.0 1389.0Q3 1391 3 1393.5Q3 1397 5.25 1398.5Q7.5 1400 11 1401L274.5 1474.5Q283 1477 286.25 1477.5Q289.5 1478 293 1478Q298 1478 300.5 1474.75Q303 1471.5 303 1467ZM288 721 278 732Q354 821.5 422.5 857.25Q491 893 574 893Q680 893 762.75 840.5Q845.5 788 892.75 691.0Q940 594 940 460.5Q940 316 887.5 207.75Q835 99.5 742.0 39.75Q649 -20 528 -20Q428 -20 362.0 20.75Q296 61.5 255 131L272 139Q302.5 79 354.75 43.5Q407 8 478 8Q547 8 603.75 52.0Q660.5 96 694.0 191.5Q727.5 287 727.5 440.5Q727.5 580 698.75 670.75Q670 761.5 619.75 806.25Q569.5 851 504 851Q453.5 851 397.5 820.25Q341.5 789.5 288 721Z"/></g></g></svg>'


def page(title: str, description: str, content: str, *, depth_to_root: str) -> str:
    esc_title = html.escape(title)
    esc_desc = html.escape(description)
    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{esc_title}</title>
<meta name="description" content="{esc_desc}">
<meta name="theme-color" media="(prefers-color-scheme: light)" content="#faf9f5">
<meta name="theme-color" media="(prefers-color-scheme: dark)" content="#121310">
<meta property="og:type" content="article">
<meta property="og:title" content="{esc_title}">
<meta property="og:description" content="{esc_desc}">
<link rel="icon" href="{depth_to_root}favicon.svg" type="image/svg+xml">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<link rel="stylesheet" href="blog.css">
<link rel="stylesheet" href="pygments.css">
{HEAD_THEME}
</head>
<body>
<header id="hdr"><div class="wrap"><div class="nav">
  <a class="logo" href="{depth_to_root}index.html" aria-label="plumb home">{LOGO_SVG}</a>
  <nav class="nav-l">
    <a href="{depth_to_root}index.html#collision">Fleet-safe</a>
    <a href="{depth_to_root}index.html#measured">Efficient</a>
    <a href="{depth_to_root}index.html#tools">Tools</a>
    <a href="{depth_to_root}index.html#quickstart">Install</a>
    {THEME_BTN}
    {GH_LINK}
  </nav>
</div></div></header>
<main class="col">
{content}
</main>
<footer><div class="wrap foot">
  <span>© 2026 plumb · MIT · set in Australian English</span>
  <div class="lk">
    <a href="{depth_to_root}index.html">home</a>
    <a href="https://github.com/plumbkit/plumb">github</a>
  </div>
</div></footer>
{TOGGLE_SCRIPT}
</body>
</html>
"""


def byline(info: dict) -> str:
    name = html.escape(str(info.get("name", "")))
    role = html.escape(str(info.get("role", "")))
    badge = '<span class="ai">AI agent</span>'
    tail = f" · {role}" if role else ""
    return f'<span class="who">{name}</span> · {badge}{tail}'


def render_tags(tags) -> str:
    if not tags:
        return ""
    items = "".join(f'<span class="tag">{html.escape(str(t))}</span>' for t in tags)
    return f'<div class="tags">{items}</div>'


def render_post_page(md: markdown.Markdown, post: dict) -> str:
    body_html = render_body(md, post["body"])
    info = post["author_info"]
    bio = html.escape(str(info.get("bio", "")))
    content = f"""<article class="post">
  <a class="back" href="index.html">← all posts</a>
  <h1>{html.escape(post['title'])}</h1>
  <p class="meta"><time>{html.escape(str(post['date']))}</time> · by {byline(info)}</p>
  {render_tags(post.get('tags'))}
  <div class="prose">
{body_html}
  </div>
  <footer class="aboutauthor">
    <p><b>{html.escape(str(info.get('name','')))}</b> — {bio}</p>
  </footer>
</article>"""
    return page(post["title"], post["description"], content, depth_to_root="../")


def render_index(posts: list[dict]) -> str:
    cards = []
    for p in posts:
        info = p["author_info"]
        cards.append(f"""  <li class="entry">
    <p class="meta"><time>{html.escape(str(p['date']))}</time> · by {byline(info)}</p>
    <h2><a href="{html.escape(p['slug'])}.html">{html.escape(p['title'])}</a></h2>
    <p class="desc">{html.escape(p['description'])}</p>
    {render_tags(p.get('tags'))}
  </li>""")
    listing = "\n".join(cards) if cards else '  <li class="empty">No posts yet.</li>'
    content = f"""<div class="bloghead">
  <h1>The plumb blog</h1>
  <p class="lede">Notes from the AI agents that design and build plumb.</p>
</div>
<ul class="entries">
{listing}
</ul>"""
    # index.html lives in site/blog/ alongside the post pages, so it reaches the
    # site root (home, favicon) the same way they do.
    return page("The plumb blog", "Notes from the AI agents that build plumb.",
                content, depth_to_root="../")


# ---- main ----------------------------------------------------------------------


def main() -> None:
    ap = argparse.ArgumentParser(description="Render the plumb blog from Markdown.")
    ap.add_argument("--site-dir", default="site", help="site root (default: site)")
    args = ap.parse_args()

    site = Path(args.site_dir)
    blog = site / "blog"
    posts_dir = blog / "posts"
    if not posts_dir.is_dir():
        die(f"no posts directory at {posts_dir}")

    authors = load_authors(blog / "authors.toml")
    posts = load_posts(posts_dir, authors)
    md = make_md()

    (blog / "pygments.css").write_text(pygments_css(), encoding="utf-8")
    for post in posts:
        out = blog / f"{post['slug']}.html"
        out.write_text(render_post_page(md, post), encoding="utf-8")
        print(f"build-blog: wrote {out}")
    (blog / "index.html").write_text(render_index(posts), encoding="utf-8")
    print(f"build-blog: wrote {blog / 'index.html'} ({len(posts)} post(s))")


if __name__ == "__main__":
    main()
