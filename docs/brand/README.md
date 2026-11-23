# Scruples brand assets

Served at `/static/brand/...` (embedded in the binary; cached for a year with `immutable` when the URL carries `?v=`, otherwise for a day). This README and `concepts/` live in `docs/brand/` so they are not embedded or served.

## Active logo

**Concept 3, the crooked compass**: an "S" made of an emerald YES arrow and a rose NO arrow inside an indigo compass ring with an amber star at north.

| File | Use |
| --- | --- |
| `logo.svg` | The mark (transparent background); used in the nav and on login/signup |
| `favicon.svg` | Mark on a dark rounded square; browser tab icon |
| `favicon-32.png` | 32px favicon fallback (also served at `/favicon.ico`) |
| `apple-touch-icon.png` | 180px iOS home-screen icon |
| `icon-192.png`, `icon-512.png` | Web manifest icons (full-bleed, maskable-safe) |
| `site.webmanifest` | Web app manifest (name Scruples, dark theme color) |
| `raccoon.svg` | Mascot (concept 4) for empty states |

## Concepts

The four original drafts live in `concepts/` (resized to 512px):

1. `concept-1-propped-halo.png`: a card with devil horns propping up a halo.
2. `concept-2-trolley-fork.png`: a trolley track switch splitting into YES and NO rails.
3. `concept-3-crooked-compass.png`: **active**.
4. `concept-4-guilty-raccoon.png`: **mascot**, the side-eye raccoon (`raccoon.svg`).

## Switching the active logo

1. Redraw the chosen concept as a clean SVG in a 64x64 `viewBox` and overwrite `logo.svg` (transparent background).
2. Do the same for `favicon.svg` (put it on a `#0f172a` rounded square), then re-export `favicon-32.png`, `apple-touch-icon.png` (180px), `icon-192.png` and `icon-512.png` from it.
3. Bump the `?v=` query on the brand URLs in `templates/layout.html` (and `login.html`/`signup.html`). Static files are cached for a year, so browsers will not refetch a changed file without a new URL.
