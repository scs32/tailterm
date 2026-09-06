# Appearance references

Tailterm uses straight terminal frames: square edges, thin dividers, compact monospace popup headers, and the same hover treatment throughout the workspace. Theme, typeface, font size, cursor and spacing remain independent. This is still an xterm-based browser terminal, not the Ghostty renderer or a Ghostty configuration parser.

Sources consulted:

- [Ghostty configuration reference](https://ghostty.org/docs/config/reference)
- [Ghostty keybinding actions](https://ghostty.org/docs/config/keybind/reference)
- [Catppuccin Ghostty](https://github.com/catppuccin/ghostty)
- [Tokyo Night Ghostty palette](https://github.com/folke/tokyonight.nvim/blob/main/extras/ghostty/tokyonight_night)
- [Rosé Pine Ghostty](https://github.com/rose-pine/ghostty)
- [tmux clipboard documentation](https://github.com/tmux/tmux/wiki/Clipboard)

Palettes are curated familiar choices, not a claimed popularity ranking. JetBrains Mono and IBM Plex Mono ship locally from Fontsource packages. Cascadia (CaskaydiaCove Nerd Font Mono) and FiraCode Nerd Font Mono ship locally with regular and bold weights. System Mono remains the default; existing Fira selections now use the Nerd Font edition. No font service or analytics request is needed. Font packages include their open font license files. Ligature shaping is not implemented by this terminal renderer.


## Readability and preview

Appearance includes a real xterm preview with normal and dim output, success/warning/error text, application-supplied dark and light backgrounds, Nerd Font symbols and an inverse tmux-style status line. The same renderer options apply to connected terminals. Minimum contrast correction is enabled; Dawn also uses darker accent and ANSI colors. The DOM renderer's dim ANSI fallback colors stay opaque because its usual alpha reduction happens after contrast checking. Muted UI text is adjusted separately. Applications can still supply their own colors, but foreground contrast is corrected against their backgrounds.

Font cards identify the family, show distinguishable characters and symbol samples, and describe local availability. Programming ligatures are not shaped. Nerd Font Mono editions retain the complete upstream character map and fit symbols to terminal cells. Font downloads occur from Tailterm's own origin and are cached normally by the browser.

## Tab appearance

Open a tab's **… → Tab appearance** to set a local label, emoji/symbol, color marker and label typeface. Colors mark the edge rather than coloring text, preserving readability. Reset restores the defaults; Save applies the choice. The terminal font stays independent. Group tabs use the focused pane's color and typeface and include the member labels. Decorations survive reconnects and encrypted workspace restoration in this browser; they do not rename remote tmux sessions.

## Bundled Nerd Fonts

Source: [Nerd Fonts v3.5.1](https://github.com/ryanoasis/nerd-fonts/releases/tag/v3.5.1), official `CascadiaCode.tar.xz` and `FiraCode.tar.xz` assets. `CaskaydiaCoveNerdFontMono-{Regular,Bold}.ttf` and `FiraCodeNerdFontMono-{Regular,Bold}.ttf` were converted to WOFF2 using fontTools with no subsetting. Original licenses are in `client/fonts/` and copied to the deployed `licenses/` directory.

Verification: `node tests/appearance-browser.mjs` checks sample terminal and popup contrast in every theme, font loading, and mobile bounds in Chromium and WebKit. `npm run test:static` exercises actual popup layouts and tab decoration persistence. `node tests/tooltip-rendering.mjs` checks shared hover styling, focus-to-hover transitions and that terminal redraws do not trigger tooltip scans.

Workspace buttons share a real one-pixel border, square corners, terminal font, readable colors, and the same hover/focus treatment across the header, session toolbar, sidebar, footer, launcher and mobile keyboard bar. Selected states retain their accent border, primary actions retain their fill, and disabled actions stay visually disabled on hover. The built-app control audit in `tests/workspace-controls-browser.mjs` compares every visible control with Tailscale in light/dark desktop/mobile layouts and captures each toolbar.

## Additional Tailterm palettes and typeface

Ember, Lagoon, and Voltage are original Tailterm palettes. Ember pairs warm charcoal with amber; Lagoon uses deep blue with teal; Voltage pairs a violet background with saturated pink, cyan, and lime. All use the same terminal contrast correction and are included in the Chromium/WebKit readability checks.

Source Code Pro is bundled locally from `@fontsource/source-code-pro` with regular and bold Latin weights; its license is included with deployed font licenses. It is available for terminal text and tab labels. It is not a Nerd Font edition.

Popup headers cover the dialog’s full padded width and remain opaque all the way to its top edge while scrolling, on desktop and mobile.
