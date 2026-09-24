// Oracle that extracts the checkout index's embedded script and stylesheet,
// exactly as render.ts builds them, into internal/checkoutindex/assets/. Go
// embeds them with go:embed rather than reconstructing their text itself:
// liveUpdateScript() embeds `planIndexUpdate.toString()`, whose exact bytes
// come from Node's own type-stripping, which nothing outside Node can
// reproduce by translation. Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/index-assets.ts
//
// Unlike every other oracle here, this one writes its own files directly
// (internal/checkoutindex/assets/script.js and styles.css) rather than to
// stdout: its output is exactly the bytes go:embed reads, not JSON testdata.
import { writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { liveUpdateScript, renderIndexHtml } from '../../src/checkout-index/render.ts';
import { placeholderSnapshot } from '../../src/checkout-index/snapshot.ts';

const script = liveUpdateScript();

// STYLES itself is not exported by render.ts; pulled back out of a rendered
// page, the one place it appears verbatim, rather than changing src/ (frozen
// apart from fixes) to export it.
const rendered = renderIndexHtml(placeholderSnapshot('1970-01-01T00:00:00.000Z'));
const styleOpen = '<style>';
const styleClose = '</style>';
const start = rendered.indexOf(styleOpen) + styleOpen.length;
const end = rendered.indexOf(styleClose, start);
if (start < styleOpen.length || end < 0) throw new Error('could not find <style>...</style> in a rendered page');
// renderIndexHtml writes `<style>${STYLES}  </style>` — two literal spaces
// between the stylesheet and the closing tag — so strip exactly that back
// off to recover STYLES itself, which is what the shell fingerprint hashes.
const styles = rendered.slice(start, end).replace(/ {2}$/, '');

const here = path.dirname(fileURLToPath(import.meta.url));
const assets = path.join(here, '..', '..', 'internal', 'checkoutindex', 'assets');
writeFileSync(path.join(assets, 'script.js'), script);
writeFileSync(path.join(assets, 'styles.css'), styles);
process.stdout.write(`wrote ${assets}/script.js (${String(script.length)} chars) and styles.css (${String(styles.length)} chars)\n`);
