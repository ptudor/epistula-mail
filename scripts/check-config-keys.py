#!/usr/bin/env python3
"""Ensure every supported TOML tag is documented in both shipped configs."""
from pathlib import Path
import re
import sys

root = Path(__file__).resolve().parent.parent
missing = []
for module in ("database", "imap", "api", "llm-worker", "mcp"):
    source = root / module / "config.go"
    tags = set(re.findall(r'toml:"([^",]+)', source.read_text()))
    for relative in (f"{module}/epistula-{module}.toml.example",
                     f"packaging/epistula-{module}.toml"):
        text = (root / relative).read_text()
        for tag in sorted(tags):
            # Accept real or commented assignments and section declarations,
            # including dotted ([submission.accounts]) and array-of-tables
            # ([[submission.accounts]]) forms whose last component is the tag.
            section = rf"(?:[A-Za-z0-9_-]+\.)*{re.escape(tag)}"
            pattern = rf"^\s*(?:#\s*)?(?:{re.escape(tag)}\s*=|\[{section}\]|\[\[{section}\]\])"
            if not re.search(pattern, text, re.MULTILINE):
                missing.append(f"{relative}: missing TOML key/section {tag}")
if missing:
    print("\n".join(missing), file=sys.stderr)
    sys.exit(1)
print("All configuration tags appear in example and package TOMLs.")
