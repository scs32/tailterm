export function setupTabStrip() {
  const strip = document.querySelector(".tab-strip"),
    viewport = document.querySelector("#tabs");
  const left = document.querySelector("#tabs-left"),
    right = document.querySelector("#tabs-right");
  let frame;
  function update() {
    const tab = viewport.querySelector(".tab");
    const minimum = tab ? parseFloat(getComputedStyle(tab).minWidth) : 0;
    const overflow =
      !!tab && viewport.children.length * minimum > strip.clientWidth + 1;
    left.hidden = right.hidden = !overflow;
    left.disabled = viewport.scrollLeft <= 1;
    right.disabled =
      viewport.scrollLeft + viewport.clientWidth >= viewport.scrollWidth - 1;
  }
  function reveal() {
    const active = viewport.querySelector(".tab.active");
    if (!active) return;
    const tab = active.getBoundingClientRect(),
      box = viewport.getBoundingClientRect();
    if (tab.left < box.left) viewport.scrollLeft -= box.left - tab.left;
    else if (tab.right > box.right)
      viewport.scrollLeft += tab.right - box.right;
    update();
  }
  left.onclick = () =>
    viewport.scrollBy({
      left: -Math.max(110, viewport.clientWidth * 0.7),
      behavior: "smooth",
    });
  right.onclick = () =>
    viewport.scrollBy({
      left: Math.max(110, viewport.clientWidth * 0.7),
      behavior: "smooth",
    });
  viewport.addEventListener("scroll", update, { passive: true });
  viewport.addEventListener(
    "wheel",
    (e) => {
      if (
        viewport.scrollWidth <= viewport.clientWidth ||
        Math.abs(e.deltaX) >= Math.abs(e.deltaY)
      )
        return;
      e.preventDefault();
      viewport.scrollLeft +=
        e.deltaY *
        (e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? viewport.clientWidth : 1);
    },
    { passive: false },
  );
  viewport.addEventListener("keydown", (e) => {
    const target = e.target.closest("[data-tab]");
    if (!target || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key))
      return;
    const buttons = [...viewport.querySelectorAll("[data-tab]")],
      index = buttons.indexOf(target);
    const next =
      e.key === "Home"
        ? buttons[0]
        : e.key === "End"
          ? buttons.at(-1)
          : buttons[
              Math.max(
                0,
                Math.min(
                  buttons.length - 1,
                  index + (e.key === "ArrowRight" ? 1 : -1),
                ),
              )
            ];
    e.preventDefault();
    const id = next.dataset.tab;
    next.click();
    viewport
      .querySelector(`[data-tab="${id}"]`)
      ?.focus({ preventScroll: true });
    reveal();
  });
  const observer = new ResizeObserver(() => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => {
      update();
      reveal();
    });
  });
  observer.observe(strip);
  observer.observe(viewport);
  return { update, reveal };
}
