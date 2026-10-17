# Brand

treerig's visual identity: the mark, its small-size version, lockups, colour,
type and voice, and notes for wiring it into the project. Open
[`brand-book.html`](brand-book.html) in a browser for the illustrated version.
[`decisions.md`](decisions.md) records how the name and the mark were chosen.

| | |
| --- | --- |
| Name | treerig, always lower case. Capitalise only to start a sentence. |
| Command | `treerig`, with `trig` proposed as a short alias (not shipped yet; see [Integration](#integration)). |
| Line | A rig for every branch. |
| Mark | The Detached Fork. |
| Colour | Evergreen `#15271E`, Paper `#F3F5F1`, Signal Red `#C4362C` (`#F0715F` on dark). |
| Type | Archivo (display), IBM Plex Sans (body), IBM Plex Mono (code). |

## Files

| File | Use |
| ---- | --- |
| `treerig-mark.svg` | The mark on light grounds, 20 px and up. |
| `treerig-mark-dark.svg` | The mark on dark grounds, 20 px and up. |
| `treerig-favicon-16.svg` | The mark at 16 px, light grounds. Pixel-built; use it only at 16 px. |
| `treerig-favicon-16-dark.svg` | The same, for dark grounds. |
| `treerig-icon.svg` | App icon: the mark on an evergreen rounded square. |
| `treerig-logo.svg` | Lockup (mark and wordmark) on light grounds. |
| `treerig-logo-dark.svg` | Lockup on dark grounds. |
| `treerig-social-preview.png` | GitHub social preview, 1280 × 640. |
| `brand-book.html` | The brand book. Loads its fonts from Google Fonts. |
| `source/build.py` | Regenerates every SVG above. Python 3 standard library only. |

The SVGs are generated. Change the geometry or colours in `source/build.py`,
then run `python3 docs/brand/source/build.py`; don't hand-edit the SVGs. The
wordmark is stored in the script as outlines, so the font is not needed.

## The mark

The Detached Fork reads the way a worktree works:

- **The ring** is a commit on `main`, where the path starts.
- **The ink path** is `main`, running on to its next heading.
- **The red branch** is yours: the worktree you're in.
- **The gap** between the split and your branch is isolation. Your branch
  shares history with `main` and nothing else.

Construction, on a 64-unit grid:

- Paths are 6 units wide with round caps and joins.
- The commit ring has a 4.8-unit radius and a 4.4-unit stroke, with an open centre.
- The red branch starts clear of the split point. It never touches.
- Clear space is one ring diameter on every side.

Don't:

- Close the gap. The break is the meaning.
- Make both branches red. Red is only ever your branch.
- Rotate or mirror the mark. The path reads left to right.
- Scale the master below 20 px.

### Small sizes

At 16 px the master blurs: the ring fills in and the gap closes.
`treerig-favicon-16.svg` draws the same mark pixel by pixel. It has a 1 px
ring and arrowheads, doubled diagonal shafts, and one empty pixel kept at the
break.

- At 16 px, use the 16 px file.
- From 20 px up, use the master. The ring opens fully from 24 px.
- If a 20–24 px placement matters (some toolbars, the Windows taskbar), add a
  hand-tuned 24 px file the same way rather than scaling.

### Lockups and icon

The wordmark is Archivo ExtraBold at width 112, converted to outlines. The
mark sits on the baseline to the left of it. Don't re-set the wordmark in
another face, and don't move the mark to the right. The app icon is the dark
mark on an evergreen rounded square.

## Colour

| Name | Hex | Use |
| ---- | --- | --- |
| Evergreen | `#15271E` | Ink: the mark, text, dark grounds. |
| Paper | `#F3F5F1` | Light grounds; the mark on dark. |
| Signal Red | `#C4362C` | Your branch, and nothing else. `#F0715F` on dark grounds. |
| Night | `#0E1B15` | Dark grounds behind the mark. |
| Moss | `#5C6F62` | Secondary text, rules, captions. |

Evergreen and Paper carry nearly everything. Red is used sparingly and always
means one thing, yours; if it appears anywhere else it stops meaning that.

State colours, for the CLI and the checkout index. They are proposed and not in
the code yet. They stay apart from Signal Red so that red keeps its one meaning.

| State | Hex |
| ----- | --- |
| ready | `#1F8A5B` |
| pending | `#9A6A0C` |
| missing | `#7A2E3A` |

## Type

All three faces are on Google Fonts under the SIL Open Font License.

- **Archivo**, 800, width 112: headings and the wordmark.
- **IBM Plex Sans**, 400 and 600: body text.
- **IBM Plex Mono**, 400 and 500: code, commands, data.

## Voice

Write the way the README already does: plain and exact, saying what happened,
what is guaranteed and what to do next. The name may play on rigs; help text,
errors and docs never do. In output, use the product's real nouns (worktree,
lock, home, container), and no puns, jokes or emoji. Output should read the
same in a CI log.

## Integration

Notes for wiring the identity into the project. None of this is done yet.

1. **README header.** Replace the `# treerig` heading with the lockup, so
   GitHub shows the dark version in dark mode:

   ```html
   <h1>
     <picture>
       <source media="(prefers-color-scheme: dark)" srcset="docs/brand/treerig-logo-dark.svg">
       <img alt="treerig" src="docs/brand/treerig-logo.svg" height="56">
     </picture>
   </h1>
   ```

2. **Social preview.** Upload `treerig-social-preview.png` under the GitHub
   repository's Settings → General → Social preview. GitHub can't read it from
   the repository.

3. **Checkout index favicon.** The index page is rendered by
   `internal/checkoutindex/render.go`, with its styles in
   `internal/checkoutindex/assets/styles.css`. It has no favicon today. Adding
   `treerig-favicon-16.svg` (and the master for larger sizes) as
   `<link rel="icon">` data URIs keeps the page self-contained. The index
   uses its own neutral palette; adopting the brand colours there is optional,
   and its HTML is pinned by golden tests (`internal/checkoutindex/golden_test.go`).

4. **The `trig` alias** was chosen during naming but is not implemented.
   `brand.CLIName` already follows the name the binary was invoked as, so the
   alias needs only a second name on disk:
   - Homebrew: in `packaging/homebrew/treerig.rb`, add
     `bin.install_symlink "treerig" => "trig"` to `install`.
   - `install.sh`: create a `trig` symlink beside `treerig`, but skip it and
     say so if another `trig` is already on `PATH`.
   - The Debian package in `packaging/debian` can ship the same symlink.

   Known name-sharers for `trig`, checked in October 2026: an npm package
   `trig` from 2014 that installs a `trig` command, and a small Go helper for
   CodeKit (`zengabor/trig`). Neither is in Homebrew core or cask, Ubuntu,
   Nixpkgs or Scoop, and oh-my-zsh, Prezto and tldr-pages have no `trig`.

5. **Domains.** `treerig.dev` and `treerig.com` had no DNS records when checked
   in October 2026. That suggests they were unregistered, but registration was
   not verified with a registrar.

6. **The previous-name guard.** `internal/style/branding_test.go` fails if
   any tracked file's path or contents carries the product's previous name in
   any letter case, binary files included. Keep it out of everything here,
   including regenerated PNGs.
