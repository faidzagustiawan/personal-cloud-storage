export type Theme = "system" | "light" | "dark";

const KEY = "cloud.theme";

export function readTheme(): Theme {
  try {
    const stored = localStorage.getItem(KEY);
    if (stored === "light" || stored === "dark" || stored === "system") return stored;
  } catch {
    // Private browsing and blocked site data both throw here. The system
    // default is a perfectly good answer.
  }
  return "system";
}

export function applyTheme(theme: Theme): void {
  const root = document.documentElement;
  // "system" removes the stamp entirely so prefers-color-scheme decides.
  if (theme === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", theme);
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    // A theme that does not persist is a small loss; a crash is not.
  }
}
