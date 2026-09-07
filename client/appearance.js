import { IDLE_MINUTES } from "./inactivity.js";
const palette = (name, background, foreground, accent, colors) => ({
  name,
  background,
  foreground,
  accent,
  terminal: {
    background,
    foreground,
    cursor: accent,
    cursorAccent: background,
    selectionBackground: accent + "44",
    selectionForeground: foreground,
    ...Object.fromEntries(
      [
        "black",
        "red",
        "green",
        "yellow",
        "blue",
        "magenta",
        "cyan",
        "white",
        "brightBlack",
        "brightRed",
        "brightGreen",
        "brightYellow",
        "brightBlue",
        "brightMagenta",
        "brightCyan",
        "brightWhite",
      ].map((key, i) => [key, colors[i]]),
    ),
  },
});
// Inspired by the projects' Ghostty palettes; provenance is in docs/appearance.md.
export const themes = {
  ember: palette("Ember", "#1c1410", "#f5e6d3", "#ffb454", [
    "#30231c",
    "#ff8f85",
    "#b9d88c",
    "#ffd080",
    "#9ec9ef",
    "#dcb0ea",
    "#8fd5c5",
    "#eee0cb",
    "#a48a78",
    "#ffaaa2",
    "#d1eca3",
    "#ffe0a3",
    "#bedfff",
    "#efcaff",
    "#b0efdf",
    "#fff5e7",
  ]),
  lagoon: palette("Lagoon", "#081d29", "#d8eef3", "#48dfc6", [
    "#143442",
    "#ff929e",
    "#85dca6",
    "#e9cf85",
    "#83bfff",
    "#c5a6f2",
    "#67dbe0",
    "#d2e9ed",
    "#759baa",
    "#ffb0b9",
    "#acf2c4",
    "#ffe6a8",
    "#abd8ff",
    "#ddc7ff",
    "#a1f4ef",
    "#f0fcff",
  ]),
  voltage: palette("Voltage", "#180b2d", "#f5eaff", "#ff69dc", [
    "#32184c",
    "#ff779c",
    "#a6f56b",
    "#ffe66b",
    "#8db7ff",
    "#ef8cff",
    "#55efff",
    "#f0ddff",
    "#ac89c4",
    "#ffa1bc",
    "#c7ff99",
    "#fff3a3",
    "#b5d1ff",
    "#ffb0ee",
    "#a0faff",
    "#ffffff",
  ]),
  tailserve: palette("Tailterm", "#101311", "#d2dbd4", "#b9ecc4", [
    "#171d19",
    "#ed8e8e",
    "#a3d9ac",
    "#dfc68b",
    "#9bbdeb",
    "#c1a5dc",
    "#91c9c4",
    "#e1e9e2",
    "#65766a",
    "#f6aaaa",
    "#b9ecc4",
    "#eed6a1",
    "#b5d0f5",
    "#d7bfef",
    "#aee2dc",
    "#f1f6f2",
  ]),
  catppuccin: palette("Catppuccin Mocha", "#1e1e2e", "#cdd6f4", "#cba6f7", [
    "#45475a",
    "#f38ba8",
    "#a6e3a1",
    "#f9e2af",
    "#89b4fa",
    "#f5c2e7",
    "#94e2d5",
    "#bac2de",
    "#585b70",
    "#f38ba8",
    "#a6e3a1",
    "#f9e2af",
    "#89b4fa",
    "#f5c2e7",
    "#94e2d5",
    "#a6adc8",
  ]),
  tokyo: palette("Tokyo Night", "#1a1b26", "#c0caf5", "#7aa2f7", [
    "#15161e",
    "#f7768e",
    "#9ece6a",
    "#e0af68",
    "#7aa2f7",
    "#bb9af7",
    "#7dcfff",
    "#a9b1d6",
    "#414868",
    "#f7768e",
    "#9ece6a",
    "#e0af68",
    "#7aa2f7",
    "#bb9af7",
    "#7dcfff",
    "#c0caf5",
  ]),
  rose: palette("Rosé Pine", "#191724", "#e0def4", "#ebbcba", [
    "#26233a",
    "#eb6f92",
    "#31748f",
    "#f6c177",
    "#9ccfd8",
    "#c4a7e7",
    "#ebbcba",
    "#e0def4",
    "#6e6a86",
    "#eb6f92",
    "#31748f",
    "#f6c177",
    "#9ccfd8",
    "#c4a7e7",
    "#ebbcba",
    "#e0def4",
  ]),
  nord: palette("Nord", "#2e3440", "#d8dee9", "#88c0d0", [
    "#3b4252",
    "#bf616a",
    "#a3be8c",
    "#ebcb8b",
    "#81a1c1",
    "#b48ead",
    "#88c0d0",
    "#e5e9f0",
    "#4c566a",
    "#bf616a",
    "#a3be8c",
    "#ebcb8b",
    "#81a1c1",
    "#b48ead",
    "#8fbcbb",
    "#eceff4",
  ]),
  dawn: palette("Rosé Pine Dawn", "#faf4ed", "#575279", "#705588", [
    "#f2e9e1",
    "#b4637a",
    "#286983",
    "#946000",
    "#326f79",
    "#907aa9",
    "#a24e51",
    "#575279",
    "#706980",
    "#b4637a",
    "#286983",
    "#946000",
    "#326f79",
    "#907aa9",
    "#a24e51",
    "#575279",
  ]),
};
export const fonts = {
  source: {
    name: "Source Code Pro",
    family: '"Source Code Pro", monospace',
    description:
      "Open, balanced letterforms with clear punctuation. Regular and bold weights bundled locally for consistent rendering.",
  },
  cascadia: {
    name: "Cascadia · Nerd Font",
    family: '"CaskaydiaCove Nerd Font Mono", monospace',
    description:
      "CaskaydiaCove Mono. Rounded forms, distinct 0/O and 1/l. Includes Nerd Font and Powerline symbols.",
    nerd: true,
  },
  system: {
    name: "System Mono",
    family: '"SFMono-Regular", Menlo, Consolas, monospace',
    description:
      "Uses your device’s monospace font. The exact face and symbol coverage vary by device.",
  },
  jetbrains: {
    name: "JetBrains Mono",
    family: '"JetBrains Mono", monospace',
    description:
      "Tall lowercase letters and clear punctuation. Bundled for consistent rendering.",
  },
  ibm: {
    name: "IBM Plex Mono",
    family: '"IBM Plex Mono", monospace',
    description:
      "Compact, structured letterforms. Bundled for consistent rendering.",
  },
  fira: {
    name: "Fira Code · Nerd Font",
    family: '"FiraCode Nerd Font Mono", monospace',
    description:
      "FiraCode Mono. Open letterforms and distinct punctuation. Includes Nerd Font and Powerline symbols.",
    nerd: true,
  },
};
const defaults = {
  idleMinutes: 15,
  theme: "tailserve",
  font: "system",
  fontSize: 14,
  lineHeight: 1.25,
  cursorStyle: "block",
  cursorBlink: true,
  padding: 16,
  focusFollowsMouse: true,
  autoHistory: true,
  copyOnSelect: false,
  remoteClipboard: true,
  attentionSound: false,
  gpuRendering: true,
};
export function normalizeAppearance(value = {}) {
  const p = { ...defaults, ...value };
  if (!IDLE_MINUTES.includes(Number(p.idleMinutes))) p.idleMinutes = 15;
  else p.idleMinutes = Number(p.idleMinutes);
  if (!themes[p.theme]) p.theme = defaults.theme;
  if (!fonts[p.font]) p.font = defaults.font;
  for (const [key, min, max] of [
    ["fontSize", 10, 32],
    ["lineHeight", 1, 1.6],
    ["padding", 0, 32],
  ])
    p[key] = Number.isFinite(Number(p[key]))
      ? Math.min(max, Math.max(min, Number(p[key])))
      : defaults[key];
  if (!["block", "bar", "underline"].includes(p.cursorStyle))
    p.cursorStyle = "block";
  for (const key of [
    "cursorBlink",
    "focusFollowsMouse",
    "autoHistory",
    "copyOnSelect",
    "remoteClipboard",
    "attentionSound",
    "gpuRendering",
  ])
    p[key] = typeof p[key] === "boolean" ? p[key] : defaults[key];
  return p;
}
export function readAppearance() {
  try {
    return normalizeAppearance(
      JSON.parse(localStorage.getItem("tailserve.appearance") || "{}"),
    );
  } catch {
    return normalizeAppearance();
  }
}
export function saveAppearance(value) {
  localStorage.setItem(
    "tailserve.appearance",
    JSON.stringify(normalizeAppearance(value)),
  );
}
export function terminalAppearance(p) {
  return {
    theme: themes[p.theme].terminal,
    // Includes application-supplied RGB backgrounds; dim text retains half this ratio.
    minimumContrastRatio: 7,
    fontFamily: fonts[p.font].family,
    fontSize: p.fontSize,
    lineHeight: p.lineHeight,
    cursorStyle: p.cursorStyle,
    cursorBlink: p.cursorBlink,
  };
}
export function applyChrome(p) {
  const root = document.documentElement,
    theme = themes[p.theme];
  root.dataset.theme = p.theme;
  root.style.setProperty("--terminal-bg", theme.background);
  root.style.setProperty("--ink", theme.foreground);
  root.style.setProperty("--accent", theme.accent);
  root.style.setProperty("--terminal-padding", p.padding + "px");
  root.style.setProperty("--terminal-font", fonts[p.font].family);
  root.style.colorScheme = p.theme === "dawn" ? "light" : "dark";
  // xterm's DOM renderer halves ANSI dim alpha after its contrast check.
  // Keep fallback colors opaque; inline contrast corrections still take priority.
  let dimStyle = document.querySelector("#terminal-dim-colors");
  if (!dimStyle) {
    dimStyle = document.createElement("style");
    dimStyle.id = "terminal-dim-colors";
    document.head.append(dimStyle);
  }
  const ansi = [
    "black",
    "red",
    "green",
    "yellow",
    "blue",
    "magenta",
    "cyan",
    "white",
    "brightBlack",
    "brightRed",
    "brightGreen",
    "brightYellow",
    "brightBlue",
    "brightMagenta",
    "brightCyan",
    "brightWhite",
  ].map((key) => theme.terminal[key]);
  const cube = [0, 95, 135, 175, 215, 255];
  for (const r of cube)
    for (const g of cube)
      for (const b of cube) ansi.push(`rgb(${r},${g},${b})`);
  for (let i = 0; i < 24; i++) {
    const c = 8 + i * 10;
    ansi.push(`rgb(${c},${c},${c})`);
  }
  dimStyle.textContent =
    `.xterm .xterm-screen .xterm-rows .xterm-dim { color: ${theme.foreground}; }` +
    ansi
      .map(
        (color, i) =>
          `.xterm .xterm-screen .xterm-rows .xterm-dim.xterm-fg-${i} { color: ${color}; }`,
      )
      .join("") +
    `.xterm .xterm-screen .xterm-rows .xterm-dim.xterm-fg-257 { color: ${theme.background}; }`;
}
