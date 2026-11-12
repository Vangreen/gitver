const root = document.documentElement;
let preferred;
try { preferred = localStorage.getItem("theme"); } catch {}
if (preferred === "dark" || (!preferred && matchMedia("(prefers-color-scheme: dark)").matches)) root.dataset.theme = "dark";

document.querySelector("#theme")?.addEventListener("click", () => {
  root.dataset.theme = root.dataset.theme === "dark" ? "light" : "dark";
  try { localStorage.setItem("theme", root.dataset.theme); } catch {}
});

document.addEventListener("click", event => {
  const trigger = event.target.closest(".release-actions > summary");
  if (trigger) {
    event.preventDefault();
    const activeMenu = trigger.parentElement;
    const opening = !activeMenu.open;
    for (const menu of document.querySelectorAll(".release-actions[open]")) menu.open = false;
    activeMenu.open = opening;
    return;
  }

  for (const menu of document.querySelectorAll(".release-actions[open]")) {
    if (!menu.contains(event.target)) menu.open = false;
  }

  const button = event.target.closest(".expand");
  if (!button) return;
  const notes = button.closest(".release-footer").previousElementSibling;
  const expanded = button.getAttribute("aria-expanded") === "true";
  notes.classList.toggle("collapsed", expanded);
  button.setAttribute("aria-expanded", String(!expanded));
  button.firstChild.textContent = expanded ? "Show full changelog " : "Show less ";
});

document.addEventListener("keydown", event => {
  if (event.key !== "Escape") return;
  const menu = document.querySelector(".release-actions[open]");
  if (!menu) return;
  menu.open = false;
  menu.querySelector("summary").focus();
  event.preventDefault();
});

document.addEventListener("submit", event => {
  const menu = event.target.closest(".release-actions");
  if (menu) menu.open = false;
});

function showLoadError(event) {
  const loader = event.detail.elt?.closest(".load-more");
  const message = loader?.querySelector(".load-error");
  if (message) message.hidden = false;
}

document.body.addEventListener("htmx:responseError", showLoadError);
document.body.addEventListener("htmx:sendError", showLoadError);
document.body.addEventListener("htmx:timeout", showLoadError);
