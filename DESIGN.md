# GoBrain design system — "Instrument × Ledger"

The visual identity shared by the web UI (`internal/web/`) and the GoBrain mobile app
(Expo, separate repo — it mirrors these tokens). The web CSS
(`internal/web/static/app.css`) is the source of truth; this file is the portable spec.

**One sentence:** a research instrument — a committed petrol ground carrying the
Gopher-cyan's own hue, graph-paper texture, monospace as the status/identity voice,
sans-serif for everything the user types or reads, one glowing accent.

## Color tokens

Dark is the default; light is petrol-tinted paper, the same hue family in daylight.
An explicit user choice pins a theme; otherwise follow the OS.

| Token | Dark | Light | Role |
|---|---|---|---|
| `bg` | `#07181d` | `#eaf2f4` | page ground (carries the grid texture) |
| `bgLine` | `rgba(46,196,224,.045)` | `rgba(12,127,156,.05)` | 28px graph-paper grid lines |
| `surface` | `#0b2028` | `#ffffff` | cards: library list, hits, panels, modal |
| `elevated` | `#0f2932` | `#f2f7f8` | hover states, icon tiles, composer base |
| `raised` | `#133440` | `#ffffff` | composer top sheen (gradient `raised → elevated`) |
| `field` | `#0c242c` | `#f4f9f9` | input wells |
| `border` | `#1b3a44` | `#cfe0e4` | interactive borders |
| `borderSoft` | `#143039` | `#dfeaec` | row separators, quiet card borders |
| `ink` | `#e6f3f6` | `#0f2229` | primary text |
| `inkSoft` | `#8fb3bc` | `#48626c` | secondary text, **placeholders** (≥4.5:1 required) |
| `inkFaint` | `#5d838d` | `#7b929b` | decorative only — never running text |
| `accent` | `#2ec4e0` | `#10a8c9` | fills: primary button, brand mark, dots |
| `accentText` | `#5edcf4` | `#0c7f9c` | accent as *text* on bg/surface (contrast-tuned) |
| `accentDeep` | `#159db8` | `#087a95` | pressed/hover accent |
| `accentTint` | `rgba(46,196,224,.11)` | `rgba(16,168,201,.10)` | selected-state washes |
| `accentBorder` | `rgba(46,196,224,.38)` | `rgba(16,168,201,.32)` | selected-state borders |
| `amber` (+tint/border) | `#eab155` | `#a86a14` | in-progress (`reading` / filing) |
| `coral` (+tint/border) | `#f27d76` | `#c04a44` | errors (`misfiled`) |
| `slate` (+tint/border) | `#7e99a1` | `#5c7078` | neutral status (`queued`) |
| `onAccent` | `#032128` | `#04252c` | text on accent fills |

Status colors are for status only — never decoration. The accent marks primary
actions, selection, and identity; it never tints body copy.

## Typography

- **Sans** (system stack; Expo: SF Pro / Roboto defaults) — everything the user
  types or reads: composer, notes, titles, snippets, buttons.
- **Mono** (`ui-monospace` / SF Mono; Expo: Menlo / Roboto Mono) — the machine's
  voice: status pills, vault paths, counts, keyboard hints, token strings. Mono is
  identity, not a text style; if a human wrote it, it isn't mono.
- Scale (rem): body/input `1.0`, composer input `1.06`, row title `0.92` w550,
  section head `0.82` w650, sub/meta `0.74`, pill `0.66` mono w650 uppercase
  (letter-spacing `0.04em`). Inputs never go below 16px (iOS zoom).
- Numerals in counts: `font-variant-numeric: tabular-nums`.

## Shape, depth, texture

- Radii: `14` (cards/composer) · `10` (inputs, hits) · `7` (buttons, seg) · `999` (pills).
- The ground is not flat: 28px graph-grid (`bgLine`) over a faint top radial of
  accent-tinted bg. Panels sit on it with soft petrol shadows; the composer gets the
  big shadow + a 1px inner top hairline (`rgba(94,220,244,.07)` dark / white light).
- The primary button carries a cyan glow (`0 4px 16px -4px accent@55%`) + inner
  top highlight. Nothing else glows except the brand mark and the live conn dot.
- Expo note: the grid texture is optional on mobile — a plain `bg` with the radial
  tint reads the same; don't fake the grid with heavy images.

## Components

- **Composer (the hero):** one card — auto-growing payload textarea (Enter submits,
  Shift+Enter newline), quiet optional-note input, then a bar: segmented source-kind
  control (radio-backed, `:checked`-styled, arrow-key navigable) + primary Capture
  with a `↵` kbd hint. Attach affordance (`.attach`, dashed border) joins the bar
  with the image-upload feature.
- **Library rows:** icon tile (34px, `elevated` + border) · title (ellipsis; 2-line
  clamp on mobile) · mono sub (kind · note/path · coral error) · right meta: `open`
  link + status pill. Row hover = `elevated`.
- **Status pills:** mono uppercase, tint bg + tinted border + leading dot;
  `reading` dot pulses (1.3s opacity).
- **Search:** desktop input lives in the topbar (⌘K focuses it); on ≤560px it moves
  into the Search section. Results are quiet `surface` cards: accent-text title,
  soft snippet, faint mono path.
- **Topbar:** sticky, blurred `bg@86%`, brand mark (24px rounded square, accent,
  inner bg square) + name; theme toggle Auto/Dark/Light; connection status.
  ≤560px: mark-only brand, hide conn label + Link phone (you can't scan your own
  screen), keep disconnect.

## Motion & a11y

- 150–250ms, ease-out; state changes only — no entrance choreography. Everything
  honors `prefers-reduced-motion` (panel slide, pulse, button press).
- Focus: 2px accent outline (`outline-offset: 2px`) on every interactive element;
  seg control focuses via `:has(:focus-visible)`.
- Contrast floors: body text ≥4.5:1, placeholders ≥4.5:1 (`inkSoft`, never
  `inkFaint`), large/bold UI ≥3:1. `accentText` exists because raw `accent` fails
  as text on dark.

## Expo mapping

Ship tokens as a plain object (`tokens.ts`) keyed exactly as the table above, one
object per theme; resolve via `useColorScheme()` with the same pin-or-auto rule.
Radii/spacing/type scale carry over 1:1 (rem × 16 = dp). Use platform monospace
(`Menlo` / `monospace`) for the mono voice; system sans otherwise.
