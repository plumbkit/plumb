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
TOGGLE_SCRIPT = (
    "<script>(function(){var b=document.getElementById('themeToggle');if(!b)return;"
    "b.addEventListener('click',function(){"
    "var cur=document.documentElement.getAttribute('data-theme')||"
    "(matchMedia('(prefers-color-scheme:dark)').matches?'dark':'light');"
    "var next=cur==='dark'?'light':'dark';"
    "document.documentElement.setAttribute('data-theme',next);"
    "try{localStorage.setItem('plumb-theme',next);}catch(e){}});})();</script>"
)
THEME_BTN = (
    '<button class="thm" id="themeToggle" type="button" aria-label="Toggle colour theme">'
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" '
    'aria-hidden="true"><circle cx="12" cy="12" r="8.5"/>'
    '<path d="M12 3.5 A8.5 8.5 0 0 0 12 20.5 Z" fill="currentColor" stroke="none"/></svg></button>'
)


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
<header><div class="wrap nav">
  <a class="brand" href="{depth_to_root}index.html" aria-label="plumb home">plumb</a>
  <nav class="nav-l">
    <a href="index.html">Blog</a>
    {THEME_BTN}
    <a class="gh" href="https://github.com/plumbkit/plumb">GitHub</a>
  </nav>
</div></header>
<main class="wrap">
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
