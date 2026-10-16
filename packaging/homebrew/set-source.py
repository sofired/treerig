#!/usr/bin/env python3
"""Point a treerig Homebrew formula at a release's source archive.

Usage: set-source.py <formula> <url> <sha256>

Rewrites the first stable `url` and `sha256` lines of <formula> in place.
The release workflow's tap job runs it on the tap's Formula/treerig.rb, and
packaging/verify-snapshot.sh runs it on packaging/homebrew/treerig.rb to
produce the snapshot's formula, so both go through the same code.

The values are literal replacements taken from argv: no sed delimiter
hazard, and nothing untrusted is spliced into a program. The caller
validates them first (a strict version in the url, a 64-hex digest).
The `head` stanza is a `head` line, not a `url` line, and a bottle block's
`sha256 cellar: ...` lines don't match `sha256 "`, so both are untouched.
"""

import re
import sys


def main(argv):
    """Rewrite the formula named in argv; exit non-zero on a malformed one."""
    if len(argv) != 4:
        sys.exit("usage: set-source.py <formula> <url> <sha256>")
    path, url, sha = argv[1], argv[2], argv[3]
    with open(path) as f:
        text = f.read()
    text, n_url = re.subn(r'^(\s*)url "[^"]*"',
                          lambda m: m.group(1) + 'url "' + url + '"',
                          text, count=1, flags=re.M)
    text, n_sha = re.subn(r'^(\s*)sha256 "[^"]*"',
                          lambda m: m.group(1) + 'sha256 "' + sha + '"',
                          text, count=1, flags=re.M)
    if (n_url, n_sha) != (1, 1):
        sys.exit("expected exactly one url and one sha256 stanza in %s "
                 "(got url=%d, sha256=%d)" % (path, n_url, n_sha))
    with open(path, "w") as f:
        f.write(text)


if __name__ == "__main__":
    main(sys.argv)
