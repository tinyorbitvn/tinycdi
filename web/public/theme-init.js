// Sets <html data-theme> before the stylesheet paints, so the Orbit theme
// never flashes the wrong palette. Reads the stored preference
// ("light" | "dark" | "system") kept by src/app/theme.tsx under
// "tcdi.theme"; "system" (or no preference) resolves through
// prefers-color-scheme. External same-origin script — CSP-safe.
(function () {
  var theme = "system";
  try {
    var stored = window.localStorage.getItem("tcdi.theme");
    if (stored === "light" || stored === "dark" || stored === "system") theme = stored;
  } catch (e) {
    // Storage blocked (privacy mode): fall through to the OS preference.
  }
  if (theme === "system") {
    theme =
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark"
        : "light";
  }
  document.documentElement.setAttribute("data-theme", theme);
})();
