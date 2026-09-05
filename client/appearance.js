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
  tailserve: palette("Tailserve", "#101311", "#d2dbd4", "#b9ecc4", [
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
  dawn: palette("Rosé Pine Dawn", "#faf4ed", "#575279", "#907aa9", [
    "#f2e9e1",
    "#b4637a",
    "#286983",
    "#ea9d34",
    "#56949f",
    "#907aa9",
    "#d7827e",
    "#575279",
    "#9893a5",
    "#b4637a",
    "#286983",
    "#ea9d34",
    "#56949f",
    "#907aa9",
    "#d7827e",
    "#575279",
  ]),
};
export const fonts = {
  system: {
    name: "System Mono",
    family: '"SFMono-Regular", Menlo, Consolas, monospace',
  },
  jetbrains: { name: "JetBrains Mono", family: '"JetBrains Mono", monospace' },
  ibm: { name: "IBM Plex Mono", family: '"IBM Plex Mono", monospace' },
  fira: { name: "Fira Code", family: '"Fira Code", monospace' },
};
const defaults = {
  theme: "tailserve",
  font: "system",
  fontSize: 14,
  lineHeight: 1.25,
  cursorStyle: "block",
  cursorBlink: true,
  padding: 16,
  focusFollowsMouse: true,
  copyOnSelect: false,
  remoteClipboard: true,
};
export function normalizeAppearance(value = {}) {
  const p = { ...defaults, ...value };
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
    "copyOnSelect",
    "remoteClipboard",
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
}
