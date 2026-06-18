import { getJSON, postJSON } from "./api.js";

// themeState is a runes-based store holding the active palette. Applying it sets
// the CSS custom properties on :root so the whole UI (and charts, which read the
// same vars via cssVar) tracks the selected plumb theme.
export const themeState = $state({
  name: "plumb",
  names: [],
  palette: null,
  loaded: false,
});

function applyPalette(p) {
  if (!p) return;
  const root = document.documentElement.style;
  root.setProperty("--bg", p.bg);
  root.setProperty("--card", p.card);
  root.setProperty("--card2", p.card2);
  root.setProperty("--rule", p.rule);
  root.setProperty("--text", p.text);
  root.setProperty("--soft", p.soft);
  root.setProperty("--faint", p.faint);
  root.setProperty("--acc", p.acc);
  root.setProperty("--acc2", p.acc2);
  root.setProperty("--grn", p.grn);
  root.setProperty("--warn", p.warn);
  root.setProperty("--forest", p.forest);
  document.documentElement.style.colorScheme = p.dark ? "dark" : "light";
}

export async function loadTheme() {
  const data = await getJSON("/api/theme");
  themeState.name = data.name;
  themeState.names = data.names;
  themeState.palette = data.palette;
  themeState.loaded = true;
  applyPalette(data.palette);
}

export async function setTheme(name) {
  const data = await postJSON("/api/theme", { name });
  themeState.name = data.name;
  themeState.names = data.names;
  themeState.palette = data.palette;
  applyPalette(data.palette);
}

// cssVar reads a resolved CSS custom property — charts use this so their colours
// follow the active theme without re-importing the palette object.
export function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

// palette returns the current palette object (or a dark fallback before load).
export function palette() {
  return (
    themeState.palette || {
      bg: "#121310",
      card: "#1b1c17",
      card2: "#22231d",
      rule: "#2e2f27",
      text: "#ece8dc",
      soft: "#aaa595",
      faint: "#787465",
      acc: "#e08a55",
      acc2: "#b35a2e",
      grn: "#5cb88a",
      warn: "#d9694a",
      forest: "#2f6e4f",
      cats: ["#e08a55", "#5cb88a", "#46606c", "#6e5a7a", "#8a5e2c", "#a14a26"],
      dark: true,
    }
  );
}
