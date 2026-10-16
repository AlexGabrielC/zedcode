// Chargé de façon bloquante dans <head> : applique le thème avant le premier
// rendu (pas de flash clair → sombre).
(() => {
  let t = null;
  try { t = localStorage.getItem("zc-theme"); } catch {}
  if (t !== "light" && t !== "dark") {
    t = matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }
  document.documentElement.dataset.theme = t;
})();
