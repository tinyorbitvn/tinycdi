#!/usr/bin/env python3
"""check-doc-links.py — offline check of every relative link in the
repository's Markdown files, tracked or new (docs/superpowers/ excluded):

  * the target file or directory must exist;
  * a #fragment into a Markdown file must name one of its headings (GitHub
    slug rules) or an explicit <a id|name="..."> anchor;
  * no file outside docs/superpowers/ may link into docs/superpowers/.

    check-doc-links.py [--root DIR] [FILE...]

With FILE arguments only those files are checked. Exit 0 when every link
resolves, 1 with one "FAIL: file:line: target — reason" line per broken
link. Standard library only; http(s), mailto and other URL schemes are
skipped, so the check never touches the network.
"""
import argparse
import os
import re
import subprocess
import sys
import unicodedata
from urllib.parse import unquote

INLINE = re.compile(r'!?\[(?:[^\[\]\\]|\\.|\[[^\]]*\])*\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|\'[^\']*\'))?\s*\)')
REFDEF = re.compile(r'^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+["\'(].*)?\s*$')
HEADING = re.compile(r'^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$')
FENCE = re.compile(r'^\s{0,3}(`{3,}|~{3,})')
CODESPAN = re.compile(r'(`+)(?:(?!\1).)+?\1')
HTML_ANCHOR = re.compile(r'<a\s+[^>]*(?:id|name)\s*=\s*"([^"]+)"', re.I)
SCHEME = re.compile(r'^[a-zA-Z][a-zA-Z0-9+.-]*:')
FORBIDDEN = 'docs/superpowers/'


def slug(text):
    """GitHub heading slug: link text only, lower-case, keep letters,
    digits, '-' and '_', each space becomes '-', drop everything else."""
    text = re.sub(r'!?\[([^\]]*)\]\([^)]*\)', r'\1', text)
    text = CODESPAN.sub(lambda m: m.group(0).strip('`'), text)
    out = []
    for ch in text.strip().lower():
        if ch == ' ':
            out.append('-')
        elif ch in '-_' or unicodedata.category(ch)[0] in ('L', 'N') or unicodedata.category(ch) == 'Mn':
            out.append(ch)
    return ''.join(out)


def lines_outside_fences(path):
    """Yield (lineno, line) for lines that are not inside fenced code."""
    fence = None
    with open(path, encoding='utf-8', errors='replace') as fh:
        for n, line in enumerate(fh, 1):
            m = FENCE.match(line)
            if m:
                mark = m.group(1)
                if fence is None:
                    fence = mark[0] * 3
                elif mark.startswith(fence):
                    fence = None
                continue
            if fence is None:
                yield n, line.rstrip('\n')


_anchor_cache = {}


def anchors(path):
    if path not in _anchor_cache:
        seen, found = {}, set()
        for _, line in lines_outside_fences(path):
            for a in HTML_ANCHOR.findall(line):
                found.add(a)
            m = HEADING.match(line)
            if not m:
                continue
            base = slug(m.group(2))
            k = seen.get(base, 0)
            found.add(base if k == 0 else f'{base}-{k}')
            seen[base] = k + 1
        _anchor_cache[path] = found
    return _anchor_cache[path]


def tracked_markdown(root):
    """Tracked and untracked-but-not-ignored Markdown files that exist on
    disk, so a new doc is checked before it is committed."""
    out = subprocess.run(
        ['git', '-C', root, 'ls-files', '-z', '--cached', '--others', '--exclude-standard',
         '--', '*.md', ':(exclude,glob)docs/superpowers/**'],
        check=True, capture_output=True).stdout.decode()
    return sorted({p for p in out.split('\0') if p and os.path.isfile(os.path.join(root, p))})


def check(root, rel):
    fails = []
    src = os.path.join(root, rel)
    src_in_sp = rel.startswith(FORBIDDEN)
    for n, line in lines_outside_fences(src):
        bare = CODESPAN.sub('', line)
        targets = INLINE.findall(bare)
        m = REFDEF.match(bare)
        if m:
            targets.append(m.group(1))
        for t in targets:
            if SCHEME.match(t) or t.startswith('//'):
                continue
            path, _, frag = t.partition('#')
            path = unquote(path)
            if path == '':
                dest_rel = rel
            elif path.startswith('/'):
                dest_rel = os.path.normpath(path.lstrip('/'))
            else:
                dest_rel = os.path.normpath(os.path.join(os.path.dirname(rel), path))
            if dest_rel.startswith('..'):
                fails.append(f'FAIL: {rel}:{n}: {t} — points outside the repository')
                continue
            dest = os.path.join(root, dest_rel)
            if not os.path.exists(dest):
                fails.append(f'FAIL: {rel}:{n}: {t} — {dest_rel} does not exist')
                continue
            if not src_in_sp and (dest_rel + '/').startswith(FORBIDDEN):
                fails.append(f'FAIL: {rel}:{n}: {t} — links into {FORBIDDEN}, which is removed before release')
                continue
            if frag and dest_rel.endswith('.md') and os.path.isfile(dest):
                if unquote(frag) not in anchors(dest):
                    fails.append(f'FAIL: {rel}:{n}: {t} — no heading #{frag} in {dest_rel}')
    return fails


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--root', default=os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..'))
    ap.add_argument('files', nargs='*')
    a = ap.parse_args()
    root = os.path.abspath(a.root)
    files = a.files or tracked_markdown(root)
    fails = []
    for rel in files:
        fails += check(root, os.path.relpath(os.path.join(root, rel), root))
    for f in fails:
        print(f)
    if not fails:
        print(f'check-doc-links: {len(files)} files, every relative link resolves')
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
