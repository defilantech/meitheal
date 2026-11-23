# Meitheal logo

`generate.py` makes every logo file in `docs/images/`. The files are
generated, so change the script and rerun it rather than editing them by hand.

| File | Use |
| --- | --- |
| `meitheal-logo-icon.svg` | The knot, for 48 px and up |
| `meitheal-favicon.svg` | One-colour knot with a wider ribbon, for 16 and 32 px |
| `meitheal-logo-light.svg` | Knot, name and Ogham rule, for light backgrounds |
| `meitheal-logo-dark.svg` | The same, for dark backgrounds |
| `meitheal-social.png` | The 1280 x 640 GitHub social preview |

The README picks the light or dark lockup with `prefers-color-scheme`. The
knot's gaps are cut out, not painted, so the icon works on any background.

## What it is

- **The knot** is original geometry, not a copy of a traditional design: the
  projection of a (3,4) torus knot, a curve that winds three times around the
  centre while its radius swings four times. It is drawn as one ribbon whose
  crossings alternate over and under, as in Insular knotwork. One cord through
  four loops is the idea of a meitheal: many hands, one piece of work.
- **The name** is set in [Uncial Antiqua](https://fonts.google.com/specimen/Uncial+Antiqua)
  by Astigmatic, licensed under the SIL Open Font License 1.1, and converted to
  outlines, so the files need no font.
- **The rule under the name** spells MEITHEAL in Ogham, the early Irish
  alphabet: M, E, I, T, H, E, A, L, with a feather mark at each end.

## Colours

| Name | Hex | Use |
| --- | --- | --- |
| Ink | `#1E2A24` | Knot edge, name on light backgrounds |
| Gold | `#C9A227` | Knot ribbon |
| Gold, dark mode | `#D9B44A` | Knot and name on dark backgrounds |
| Vellum | `#F3EBDD` | Social preview background |
| Soft ink | `#4A5A52` | Secondary text |

## Regenerate

```sh
python3 -m venv /tmp/meitheal-brand && /tmp/meitheal-brand/bin/pip install fonttools
curl -fsSLo /tmp/UncialAntiqua-Regular.ttf \
  https://github.com/google/fonts/raw/90abd17b4f97671435798b6147b698aa9087612f/ofl/uncialantiqua/UncialAntiqua-Regular.ttf
/tmp/meitheal-brand/bin/python hack/brand/generate.py /tmp/UncialAntiqua-Regular.ttf docs/images
```

The font URL is pinned to a google/fonts commit, so the outlines come out the
same every time. `generate.py` also writes `meitheal-social.svg` (ignored by
git): render it at exactly 1280 x 640 in a browser to make
`meitheal-social.png`.
