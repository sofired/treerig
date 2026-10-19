#!/usr/bin/env python3
"""Generate the treerig brand SVGs and favicon PNGs in docs/brand from the
geometry below.

Run from anywhere: python3 docs/brand/source/build.py
Needs only the Python 3 standard library. The wordmark is stored here as
outlines (Archivo ExtraBold, width 112, tracking -10/1000 em), so the font is
not needed to rebuild.
"""
import os
import struct
import zlib

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..')

# Palette. Red only ever marks "your" branch.
INK, PAPER, RED, RED_DARK = '#15271E', '#F3F5F1', '#C4362C', '#F0715F'

STROKE = 6  # path weight on the 64-unit grid: 1.5 px at 16 px


def line(colour, d):
    """Return one stroked path of the mark: 6 units, round caps and joins."""
    return (f'<path d="{d}" stroke="{colour}" stroke-width="{STROKE}" '
            'stroke-linecap="round" stroke-linejoin="round" fill="none"/>')


def mark(fg, accent):
    """Return the master mark's markup on a 64x64 grid.

    main starts at a commit (the ring) and runs on into the upper branch.
    Your branch breaks away at the split, in the accent colour, and starts
    clear of the split point: the gap is the meaning and must never close.
    The group is nudged 3 units right to sit optically centred.
    """
    return ('<g transform="translate(3 0)">'
            f'<circle cx="9" cy="32" r="4.8" fill="none" stroke="{fg}" stroke-width="4.4"/>'
            + line(fg, 'M14 32H23C26 32 28 31 30 29L45 14')
            + line(fg, 'M37 11H48V22')
            + line(accent, 'M35 39L45 49')
            + line(accent, 'M37 52H48V41')
            + '</g>')


# The 16 px file: the same mark placed pixel by pixel, because at 16 px the
# master's ring fills in and its gap closes. '#' is ink, 'r' is the accent.
# 1 px ring and arrowheads; diagonal shafts doubled so they read as solid;
# one empty pixel kept between the split and your branch.
FAVICON_16 = [
    '................',
    '................',
    '...........####.',
    '.............##.',
    '............###.',
    '...........##.#.',
    '..###.....##....',
    '.#...#...##.....',
    '.#...#####......',
    '.#...#..........',
    '..###.....rr....',
    '...........rr.r.',
    '............rrr.',
    '.............rr.',
    '...........rrrr.',
    '................',
]


def favicon(fg, accent):
    """Return the 16 px pixel-art mark as crisp 1x1 rects."""
    cells = ''.join(
        f'<rect x="{x}" y="{y}" width="1" height="1" fill="{fg if ch == "#" else accent}"/>'
        for y, row in enumerate(FAVICON_16) for x, ch in enumerate(row) if ch != '.')
    return f'<g shape-rendering="crispEdges">{cells}</g>'


def png_chunk(kind, data):
    """Return one PNG chunk: length, type, data and the CRC over type and data."""
    body = kind + data
    return struct.pack('>I', len(data)) + body + struct.pack('>I', zlib.crc32(body) & 0xFFFFFFFF)


def favicon_png(fg, accent, scale):
    """Return the 16 px pixel mark as an indexed PNG, each pixel scale x scale.

    scale 1 is 16x16 and scale 2 is 32x32, the same grid at 2x for Retina
    screens. The image is a palette of transparent, ink and accent, with no
    metadata chunks, and its deflate stream is stored (level 0) rather than
    compressed: stored blocks do not depend on the zlib build, so the bytes are
    the same on every machine. The file is about a kilobyte either way.
    """
    palette = {'.': 0, '#': 1, 'r': 2}
    size = len(FAVICON_16) * scale
    raw = bytearray()
    for row in FAVICON_16:
        line = bytes([0]) + bytes(palette[ch] for ch in row for _ in range(scale))  # filter 0
        raw += line * scale
    plte = b''.join(bytes.fromhex(c.lstrip('#')) for c in ('#000000', fg, accent))
    return (b'\x89PNG\r\n\x1a\n'
            + png_chunk(b'IHDR', struct.pack('>IIBBBBB', size, size, 8, 3, 0, 0, 0))
            + png_chunk(b'PLTE', plte)
            + png_chunk(b'tRNS', bytes([0]))  # index 0 is fully transparent; the rest opaque
            + png_chunk(b'IDAT', zlib.compress(bytes(raw), 0))
            + png_chunk(b'IEND', b''))


# Wordmark outlines in font units (1000/em), baseline at y=0.
WORDMARK = 'M288 12Q229 12 189.0 -3.5Q149 -19 129.0 -52.5Q109 -86 109 -141V-405H23V-527H115L150 -683H283V-527H408V-405H283V-178Q283 -145 296.5 -127.5Q310 -110 349 -110H408V-6Q395 -1 373.0 3.0Q351 7 328.0 9.5Q305 12 288 12Z M497.0 0V-527H639.0L651.0 -442H659.0Q672.0 -472 693.0 -494.0Q714.0 -516 742.5 -528.0Q771.0 -540 804.0 -540Q823.0 -540 839.5 -537.0Q856.0 -534 868.0 -529V-384H798.0Q765.0 -384 741.0 -374.5Q717.0 -365 701.5 -346.5Q686.0 -328 678.5 -303.0Q671.0 -278 671.0 -248V0Z M1225.0 12Q1126.0 12 1054.5 -17.5Q983.0 -47 944.5 -108.0Q906.0 -169 906.0 -264Q906.0 -357 944.0 -418.0Q982.0 -479 1052.0 -509.0Q1122.0 -539 1218.0 -539Q1316.0 -539 1385.0 -510.0Q1454.0 -481 1490.0 -421.0Q1526.0 -361 1526.0 -268V-229H1085.0Q1087.0 -187 1102.5 -159.0Q1118.0 -131 1149.0 -117.0Q1180.0 -103 1226.0 -103Q1252.0 -103 1274.0 -109.0Q1296.0 -115 1313.0 -126.0Q1330.0 -137 1339.5 -152.0Q1349.0 -167 1350.0 -187H1524.0Q1524.0 -140 1503.0 -103.0Q1482.0 -66 1442.5 -40.5Q1403.0 -15 1348.0 -1.5Q1293.0 12 1225.0 12ZM1086.0 -323H1343.0Q1343.0 -348 1334.5 -366.5Q1326.0 -385 1310.0 -398.0Q1294.0 -411 1272.0 -417.5Q1250.0 -424 1224.0 -424Q1182.0 -424 1153.0 -412.0Q1124.0 -400 1107.5 -377.5Q1091.0 -355 1086.0 -323Z M1915.0 12Q1816.0 12 1744.5 -17.5Q1673.0 -47 1634.5 -108.0Q1596.0 -169 1596.0 -264Q1596.0 -357 1634.0 -418.0Q1672.0 -479 1742.0 -509.0Q1812.0 -539 1908.0 -539Q2006.0 -539 2075.0 -510.0Q2144.0 -481 2180.0 -421.0Q2216.0 -361 2216.0 -268V-229H1775.0Q1777.0 -187 1792.5 -159.0Q1808.0 -131 1839.0 -117.0Q1870.0 -103 1916.0 -103Q1942.0 -103 1964.0 -109.0Q1986.0 -115 2003.0 -126.0Q2020.0 -137 2029.5 -152.0Q2039.0 -167 2040.0 -187H2214.0Q2214.0 -140 2193.0 -103.0Q2172.0 -66 2132.5 -40.5Q2093.0 -15 2038.0 -1.5Q1983.0 12 1915.0 12ZM1776.0 -323H2033.0Q2033.0 -348 2024.5 -366.5Q2016.0 -385 2000.0 -398.0Q1984.0 -411 1962.0 -417.5Q1940.0 -424 1914.0 -424Q1872.0 -424 1843.0 -412.0Q1814.0 -400 1797.5 -377.5Q1781.0 -355 1776.0 -323Z M2310.0 0V-527H2452.0L2464.0 -442H2472.0Q2485.0 -472 2506.0 -494.0Q2527.0 -516 2555.5 -528.0Q2584.0 -540 2617.0 -540Q2636.0 -540 2652.5 -537.0Q2669.0 -534 2681.0 -529V-384H2611.0Q2578.0 -384 2554.0 -374.5Q2530.0 -365 2514.5 -346.5Q2499.0 -328 2491.5 -303.0Q2484.0 -278 2484.0 -248V0Z M2743.0 -599V-724H2917.0V-599ZM2743.0 0V-527H2917.0V0Z M3124.0 201Q3086.0 201 3053.5 186.0Q3021.0 171 3001.0 142.5Q2981.0 114 2981.0 75Q2981.0 33 3008.0 3.5Q3035.0 -26 3074.0 -41Q3046.0 -56 3031.0 -80.0Q3016.0 -104 3016.0 -133Q3016.0 -174 3045.5 -203.0Q3075.0 -232 3120.0 -240Q3082.0 -262 3058.0 -295.5Q3034.0 -329 3034.0 -369Q3034.0 -432 3069.5 -469.0Q3105.0 -506 3169.0 -522.5Q3233.0 -539 3318.0 -539Q3350.0 -539 3377.0 -536.5Q3404.0 -534 3427.0 -529Q3472.0 -553 3491.0 -579.0Q3510.0 -605 3512.0 -622H3669.0Q3668.0 -582 3652.5 -554.5Q3637.0 -527 3608.5 -510.0Q3580.0 -493 3540.0 -483Q3570.0 -462 3586.0 -432.5Q3602.0 -403 3602.0 -368Q3602.0 -309 3571.0 -271.5Q3540.0 -234 3484.5 -215.5Q3429.0 -197 3355.0 -197H3250.0Q3220.0 -197 3206.0 -188.0Q3192.0 -179 3192.0 -164Q3192.0 -150 3204.5 -140.0Q3217.0 -130 3250.0 -130H3503.0Q3584.0 -130 3625.5 -85.5Q3667.0 -41 3667.0 28Q3667.0 80 3639.0 119.0Q3611.0 158 3564.0 179.5Q3517.0 201 3460.0 201ZM3209.0 98H3434.0Q3451.0 98 3463.5 92.0Q3476.0 86 3484.0 74.0Q3492.0 62 3492.0 46Q3492.0 22 3475.5 9.0Q3459.0 -4 3436.0 -4H3210.0Q3186.0 -4 3170.0 9.0Q3154.0 22 3154.0 46Q3154.0 69 3169.5 83.5Q3185.0 98 3209.0 98ZM3318.0 -296Q3371.0 -296 3400.0 -313.5Q3429.0 -331 3429.0 -368Q3429.0 -406 3400.0 -423.0Q3371.0 -440 3318.0 -440Q3267.0 -440 3237.5 -423.0Q3208.0 -406 3208.0 -368Q3208.0 -343 3220.5 -327.0Q3233.0 -311 3258.0 -303.5Q3283.0 -296 3318.0 -296Z'
WORDMARK_X = 997  # left edge of the wordmark in the lockup, in font units


def svg(inner, size=64):
    """Wrap markup in a square SVG document."""
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {size} {size}">{inner}</svg>'


def icon():
    """Return the app-icon tile: the dark mark on an evergreen rounded square."""
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="{INK}"/>'
            f'<g transform="translate(9.6 9.6) scale(.7)">{mark(PAPER, RED_DARK)}</g></svg>')


def lockup(fg, accent):
    """Return the horizontal lockup: mark on the baseline, wordmark to its right."""
    scale = 820 / 64
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="-20 -790 {WORDMARK_X + 3669 + 60} 1010">'
            f'<g transform="translate(0 -760) scale({scale:.4f})">{mark(fg, accent)}</g>'
            f'<path fill="{fg}" transform="translate({WORDMARK_X} 0)" d="{WORDMARK}"/></svg>')


def main():
    """Write every brand SVG and favicon PNG into docs/brand."""
    files = {
        'treerig-mark.svg': svg(mark(INK, RED)),
        'treerig-mark-dark.svg': svg(mark(PAPER, RED_DARK)),
        'treerig-icon.svg': icon(),
        'treerig-favicon-16.svg': svg(favicon(INK, RED), 16),
        'treerig-favicon-16-dark.svg': svg(favicon(PAPER, RED_DARK), 16),
        'treerig-logo.svg': lockup(INK, RED),
        'treerig-logo-dark.svg': lockup(PAPER, RED_DARK),
    }
    for name, content in files.items():
        with open(os.path.join(OUT, name), 'w', encoding='utf-8', newline='\n') as f:
            f.write(content + '\n')
        print(name)
    pngs = {
        'treerig-favicon-16.png': favicon_png(INK, RED, 1),
        'treerig-favicon-16-dark.png': favicon_png(PAPER, RED_DARK, 1),
        'treerig-favicon-32.png': favicon_png(INK, RED, 2),
        'treerig-favicon-32-dark.png': favicon_png(PAPER, RED_DARK, 2),
    }
    for name, content in pngs.items():
        with open(os.path.join(OUT, name), 'wb') as f:
            f.write(content)
        print(name)


if __name__ == '__main__':
    main()
