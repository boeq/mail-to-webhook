// The drawer below `lg`: the topbar button opens and closes the
// sidebar, a scrim behind it closes it, and so does Escape. At `lg` the topbar and the
// scrim are `display: none`, so none of this is reachable there.
(() => {
  const shell = document.querySelector(".shell");
  const toggle = document.querySelector("[data-drawer-toggle]");
  const sidebar = document.getElementById("sidebar");
  if (!shell || !toggle || !sidebar) return;

  let scrim = null;

  function setOpen(open) {
    shell.classList.toggle("shell--drawer-open", open);
    toggle.setAttribute("aria-expanded", String(open));
    toggle.setAttribute("aria-label", open ? "Close menu" : "Open menu");
    toggle.querySelector('[data-drawer-icon="menu"]').hidden = open;
    toggle.querySelector('[data-drawer-icon="x"]').hidden = !open;
    if (open && !scrim) {
      scrim = document.createElement("button");
      scrim.type = "button";
      scrim.className = "scrim";
      scrim.setAttribute("aria-label", "Close menu");
      scrim.addEventListener("click", () => setOpen(false));
      sidebar.after(scrim);
    } else if (!open && scrim) {
      scrim.remove();
      scrim = null;
    }
  }

  toggle.addEventListener("click", () => {
    setOpen(!shell.classList.contains("shell--drawer-open"));
  });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && shell.classList.contains("shell--drawer-open")) {
      setOpen(false);
      toggle.focus();
    }
  });
})();
