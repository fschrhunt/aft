"""Check guide organization and repository-local inline Markdown links offline."""

import internal.session
from pathlib import Path
import re
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parent.parent


def prose(text):
    """Exclude fenced code examples from heading and link checks."""
    return re.sub(r"^```[^\n]*\n.*?^```\s*$", "", text, flags=re.M | re.S)


def anchors(text):
    """Return GitHub-style fragments for the simple headings used by these docs."""
    result = set()
    counts = {}
    for heading in re.findall(r"^#{1,6}\s+(.+?)\s*#*\s*$", prose(text), re.M):
        slug = re.sub(r"[^\w\- ]", "", heading.lower()).replace(" ", "-")
        count = counts.get(slug, 0)
        counts[slug] = count + 1
        result.add(f"{slug}-{count}" if count else slug)
    return result


def check(root):
    """Return contract failures without modifying or fetching any files."""
    errors = []
    topics = set()
    orders = {}
    for path in sorted((root / "docs/guides").rglob("*.md")):
        relative = path.relative_to(root)
        text = path.read_text()
        match = re.match(r"\A---\n(.*?)\n---\n", text, re.S)
        fields = {}
        if match:
            for line in match[1].splitlines():
                key, sep, value = line.partition(":")
                if not sep or key in fields:
                    errors.append(f"{relative}: malformed or duplicate front matter")
                fields[key] = value.strip()
        if (set(fields) != {"title", "description", "order"}
                or not all(fields.values())
                or not re.fullmatch(r"[1-9][0-9]*", fields.get("order", ""))):
            errors.append(f"{relative}: expected title, description and positive order")
            continue
        if f"# {fields['title']}\n" not in text:
            errors.append(f"{relative}: missing matching title heading")
        group = path.parent
        if group.parent != root / "docs/guides" or not (group / "README.md").is_file():
            errors.append(f"{relative}: guide needs an immediate group with README.md")
        scope = "groups" if path.name == "README.md" else str(group)
        key = (scope, int(fields["order"]))
        if key in orders:
            errors.append(f"{relative}: order duplicates {orders[key]}")
        orders[key] = relative
        if path.name != "README.md":
            if path.stem in topics:
                errors.append(f"{relative}: duplicate guide topic")
            topics.add(path.stem)
    paths = list(root.glob("*.md")) + list((root / "docs").rglob("*.md"))
    paths += list((root / ".github").rglob("*.md"))
    for path in paths:
        for target in re.findall(r"\[[^\]\n]*\]\(([^)\s]+)\)", prose(path.read_text())):
            url = urlsplit(target)
            if url.scheme or url.netloc:
                continue
            dest = (path.parent / unquote(url.path)).resolve() if url.path else path
            if url.path.startswith("/") or not dest.is_relative_to(root):
                errors.append(f"{path.relative_to(root)}: link must be repository-relative: {target}")
            elif not dest.exists():
                errors.append(f"{path.relative_to(root)}: missing link: {target}")
            elif url.fragment and dest.suffix == ".md" and unquote(url.fragment) not in anchors(dest.read_text()):
                errors.append(f"{path.relative_to(root)}: missing heading: {target}")
    return errors


if __name__ == "__main__":
    failures = check(ROOT)
    if failures:
        raise SystemExit("\n".join(failures))
    print("Documentation metadata and local links passed.")
