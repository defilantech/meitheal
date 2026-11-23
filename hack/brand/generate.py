#!/usr/bin/env python3
"""Generate Meitheal's logo files.

The knot is original geometry, not a copy of a traditional design: the
projection of a (3,4) torus knot (a curve that winds three times around the
centre while its radius swings four times), drawn as a ribbon whose crossings
alternate over and under. The wordmark is set in Uncial Antiqua (SIL Open
Font License 1.1, by Astigmatic) and converted to outlines, so the SVGs need
no font. The rule under it spells MEITHEAL in Ogham.

Usage: generate.py <UncialAntiqua-Regular.ttf> <output directory>
Needs fontTools (pip install fonttools).
"""
import math
import os
import sys

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont

INK = "#1E2A24"
GOLD = "#C9A227"
GOLD_DARK_MODE = "#D9B44A"
VELLUM = "#F3EBDD"
SOFT_INK = "#4A5A52"


def fmt(p):
    return f"{p[0]:.2f} {p[1]:.2f}"


def poly(pts, closed=False):
    return "M" + fmt(pts[0]) + "".join("L" + fmt(p) for p in pts[1:]) + ("Z" if closed else "")


# ------------------------------------------------------------------ the knot
def knot_points(cx, cy, scale, n=480, p=3, q=4, r0=2.0, amp=1.0):
    pts = []
    for i in range(n):
        t = 2 * math.pi * i / n
        r = r0 + amp * math.cos(q * t)
        a = p * t - math.pi / 2
        pts.append((cx + scale * r * math.cos(a), cy + scale * r * math.sin(a)))
    return pts


def _side(o, u, v):
    return (u[0] - o[0]) * (v[1] - o[1]) - (u[1] - o[1]) * (v[0] - o[0])


def _hit(a, b, c, d):
    d1, d2 = _side(c, d, a), _side(c, d, b)
    d3, d4 = _side(a, b, c), _side(a, b, d)
    if (d1 > 0) != (d2 > 0) and (d3 > 0) != (d4 > 0):
        return d1 / (d1 - d2)
    return None


def _unit(a, b):
    dx, dy = b[0] - a[0], b[1] - a[1]
    length = math.hypot(dx, dy)
    return (dx / length, dy / length)


def crossings_over(pts):
    """The crossings where the cord passes over, as (position, point, sin of
    the crossing angle); position is a fractional point index.

    Walking the closed curve, crossings alternate over, under, over...; Gauss's
    parity rule makes that consistent for any closed planar curve."""
    n = len(pts)
    events = []
    cid = 0
    for i in range(n):
        a, b = pts[i], pts[(i + 1) % n]
        for j in range(i + 2, n):
            if i == 0 and j == n - 1:
                continue
            c, d = pts[j], pts[(j + 1) % n]
            s = _hit(a, b, c, d)
            if s is not None:
                x = (a[0] + s * (b[0] - a[0]), a[1] + s * (b[1] - a[1]))
                events.append((i + s, cid, x, _unit(a, b)))
                events.append((j + _hit(c, d, a, b), cid, x, _unit(c, d)))
                cid += 1
    events.sort()
    tangent = {}
    for pos, c, x, u in events:
        tangent.setdefault(c, []).append(u)
    out = []
    for pos, c, x, u in events[0::2]:
        u1, u2 = tangent[c]
        out.append((pos, x, abs(u1[0] * u2[1] - u1[1] * u2[0])))
    return out


def _arc(pts, centre, length):
    """The points within an arc length of a position along the closed curve."""
    n = len(pts)
    lo = int(math.floor(centre))
    walked = 0.0
    while walked < length:
        walked += math.dist(pts[lo % n], pts[(lo - 1) % n])
        lo -= 1
    hi = int(math.ceil(centre))
    walked = 0.0
    while walked < length:
        walked += math.dist(pts[hi % n], pts[(hi + 1) % n])
        hi += 1
    return [pts[k % n] for k in range(lo, hi + 1)]


def knot(cx, cy, scale, ribbon, edge, gap, fill, outline, uid):
    """The knot as SVG elements. Gaps at the crossings are cut with a mask, so
    the background shows through and the mark works on any colour. Each cut
    is clipped to a disc around its crossing, so it can never touch a third
    strand."""
    pts = knot_points(cx, cy, scale)
    # Start the path in the middle of an over-crossing, so the join where the
    # path closes sits under a redrawn piece and can never show as a seam.
    s = int(round(crossings_over(pts)[0][0]))
    pts = pts[s:] + pts[:s]
    base = poly(pts, closed=True)
    overs = crossings_over(pts)
    outer = ribbon + 2 * edge
    clips, cuts = [], []
    for k, (pos, x, sin) in enumerate(overs):
        radius = (outer + gap) / max(sin, 0.3) + 1
        clips.append(f'<clipPath id="{uid}-c{k}"><circle cx="{x[0]:.2f}" cy="{x[1]:.2f}" r="{radius:.2f}"/></clipPath>')
        cuts.append(f'<path d="{poly(_arc(pts, pos, radius + outer))}" fill="none" stroke="#000" '
                    f'stroke-width="{outer + 2 * gap:.2f}" clip-path="url(#{uid}-c{k})"/>')
    span = scale * 3.6
    out = [f'<defs>{"".join(clips)}<mask id="{uid}" maskUnits="userSpaceOnUse" x="{cx - span:.1f}" y="{cy - span:.1f}" '
           f'width="{2 * span:.1f}" height="{2 * span:.1f}"><rect x="{cx - span:.1f}" y="{cy - span:.1f}" '
           f'width="{2 * span:.1f}" height="{2 * span:.1f}" fill="#fff"/>{"".join(cuts)}</mask></defs>',
           f'<g mask="url(#{uid})">']
    if edge:
        out.append(f'<path d="{base}" fill="none" stroke="{outline}" stroke-width="{outer:.2f}" stroke-linejoin="round"/>')
    out.append(f'<path d="{base}" fill="none" stroke="{fill}" stroke-width="{ribbon:.2f}" stroke-linejoin="round"/></g>')
    for pos, x, sin in overs:
        radius = (outer + gap) / max(sin, 0.3) + 1
        if edge:
            out.append(f'<path d="{poly(_arc(pts, pos, radius + 2))}" fill="none" stroke="{outline}" stroke-width="{outer:.2f}"/>')
        # The fill runs past the edge and has round caps, so a redrawn piece's
        # ends land on the same colour and leave no hairline.
        out.append(f'<path d="{poly(_arc(pts, pos, radius + 4))}" fill="none" stroke="{fill}" '
                   f'stroke-width="{ribbon:.2f}" stroke-linecap="round"/>')
    return "".join(out)


# ------------------------------------------------------------------ wordmark
def wordmark(font, text, x, baseline, size, colour):
    """Glyph outlines for text, left-aligned at x on baseline; returns (svg, width)."""
    glyphs = font.getGlyphSet()
    cmap = font.getBestCmap()
    upm = font["head"].unitsPerEm
    k = size / upm
    paths = []
    pen_x = x
    for ch in text:
        name = cmap[ord(ch)]
        pen = SVGPathPen(glyphs)
        glyphs[name].draw(TransformPen(pen, (k, 0, 0, -k, pen_x, baseline)))
        d = pen.getCommands()
        if d:
            paths.append(d)
        pen_x += glyphs[name].width * k
    return f'<path d="{" ".join(paths)}" fill="{colour}"/>', pen_x - x


# ------------------------------------------------------------------ Ogham
OGHAM = {"B": ("b", 1), "L": ("b", 2), "F": ("b", 3), "S": ("b", 4), "N": ("b", 5),
         "H": ("h", 1), "D": ("h", 2), "T": ("h", 3), "C": ("h", 4), "Q": ("h", 5),
         "M": ("m", 1), "G": ("m", 2), "R": ("m", 5),
         "A": ("v", 1), "O": ("v", 2), "U": ("v", 3), "E": ("v", 4), "I": ("v", 5)}


def ogham(word, x0, y, u, colour, width):
    """Ogham on a horizontal stem: B-group strokes below, H-group above,
    M-group diagonal across, vowels short across; feather marks at both ends.
    u is the unit size; returns (svg, length)."""
    step, gap, long, short = 7 * u, 12 * u, 16 * u, 7 * u
    lines = [(x0, y, x0 - 6 * u, y - 6 * u), (x0, y, x0 - 6 * u, y + 6 * u)]
    x = x0 + gap
    for ch in word:
        kind, cnt = OGHAM[ch]
        for i in range(cnt):
            xx = x + i * step
            if kind == "b":
                lines.append((xx, y, xx, y + long))
            elif kind == "h":
                lines.append((xx, y - long, xx, y))
            elif kind == "m":
                lines.append((xx - 6 * u, y + long * 0.8, xx + 6 * u, y - long * 0.8))
            else:
                lines.append((xx, y - short, xx, y + short))
        x += (cnt - 1) * step + gap
    lines += [(x, y, x + 6 * u, y - 6 * u), (x, y, x + 6 * u, y + 6 * u)]
    lines.append((x0, y, x, y))
    svg = "".join(f'<line x1="{a:.2f}" y1="{b:.2f}" x2="{c:.2f}" y2="{d:.2f}"/>' for a, b, c, d in lines)
    return (f'<g stroke="{colour}" stroke-width="{width:.2f}" stroke-linecap="round">{svg}</g>', x + 6 * u - (x0 - 6 * u))


# ------------------------------------------------------------------ files
def write(out, name, w, h, body, title):
    with open(os.path.join(out, name), "w") as f:
        f.write(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w:.0f} {h:.0f}" width="{w:.0f}" height="{h:.0f}" '
                f'role="img" aria-label="{title}"><title>{title}</title>{body}</svg>\n')


def main():
    font = TTFont(sys.argv[1])
    out = sys.argv[2]
    os.makedirs(out, exist_ok=True)

    # Icon: the full knot, gold ribbon with an ink edge, for 48 px and up.
    write(out, "meitheal-logo-icon.svg", 200, 200,
          knot(100, 100, 29, 11, 2.4, 2.2, GOLD, INK, "meitheal-icon-gaps"), "Meitheal")

    # Favicon: one colour, a wider ribbon and no edge, so it holds at 16 px.
    write(out, "meitheal-favicon.svg", 200, 200,
          knot(100, 100, 29.5, 15, 0, 4.5, GOLD, INK, "meitheal-favicon-gaps"), "Meitheal")

    # Horizontal lockups, transparent background: knot, wordmark, Ogham rule.
    for mode, word_colour, ogham_colour, gold in [("light", INK, INK, GOLD), ("dark", GOLD_DARK_MODE, GOLD_DARK_MODE, GOLD_DARK_MODE)]:
        mark = knot(100, 100, 29, 11, 2.4, 2.2, gold, INK, f"meitheal-lockup-{mode}-gaps")
        word, ww = wordmark(font, "Meitheal", 236, 108, 96, word_colour)
        rule, rl = ogham("MEITHEAL", 246, 150, 1.5, ogham_colour, 3.6)
        write(out, f"meitheal-logo-{mode}.svg", 236 + max(ww, rl) + 24, 200, mark + word + rule, "Meitheal")

    # GitHub social card, 1280 x 640 (rendered to PNG separately), centred.
    knot_r, word_size, space = 158, 112, 64
    _, ww = wordmark(font, "Meitheal", 0, 0, word_size, INK)
    _, rl = ogham("MEITHEAL", 0, 0, 1.9, INK, 4.2)
    left = (1280 - (2 * knot_r + space + max(ww, rl))) / 2
    text_x = left + 2 * knot_r + space
    mark = knot(left + knot_r, 320, 50, 19, 4.1, 3.8, GOLD, INK, "meitheal-social-gaps")
    word, _ = wordmark(font, "Meitheal", text_x, 318, word_size, INK)
    rule, _ = ogham("MEITHEAL", text_x + 12, 378, 1.9, INK, 4.2)
    tagline = (f'<text x="{text_x + 2:.1f}" y="452" font-family="Helvetica Neue, Helvetica, Arial, sans-serif" font-size="30" '
               f'fill="{SOFT_INK}">Agentic coding on models you host</text>')
    write(out, "meitheal-social.svg", 1280, 640,
          f'<rect width="1280" height="640" fill="{VELLUM}"/>' + mark + word + rule + tagline, "Meitheal")


if __name__ == "__main__":
    main()
