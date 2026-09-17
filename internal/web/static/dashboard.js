(() => {
  let activePoint;
  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.setAttribute("role", "tooltip");
  document.addEventListener("DOMContentLoaded", () => document.body.append(tooltip));

  const placeTooltip = (x, y) => {
    const gap = 14;
    const width = tooltip.offsetWidth;
    const height = tooltip.offsetHeight;
    tooltip.style.left = `${Math.min(x + gap, window.innerWidth - width - gap)}px`;
    tooltip.style.top = `${Math.max(gap, y - height - gap)}px`;
  };

  const showTooltip = (point, x, y) => {
    if (!point.dataset.tooltip) return;
    activePoint?.classList.remove("is-hovered");
    activePoint = point;
    point.classList.add("is-hovered");
    tooltip.textContent = point.dataset.tooltip;
    tooltip.classList.add("is-visible");
    placeTooltip(x, y);
  };

  const hideTooltip = () => {
    activePoint?.classList.remove("is-hovered");
    activePoint = undefined;
    tooltip.classList.remove("is-visible");
  };

  document.addEventListener("pointerover", (event) => {
    const point = event.target.closest(".chart-point[data-tooltip]");
    if (point) showTooltip(point, event.clientX, event.clientY);
  });
  document.addEventListener("pointermove", (event) => {
    if (activePoint) placeTooltip(event.clientX, event.clientY);
  });
  document.addEventListener("pointerout", (event) => {
    if (event.target.closest(".chart-point[data-tooltip]")) hideTooltip();
  });
  document.addEventListener("focusin", (event) => {
    const point = event.target.closest(".chart-point[data-tooltip]");
    if (!point) return;
    const bounds = point.getBoundingClientRect();
    showTooltip(point, bounds.left + bounds.width / 2, bounds.top);
  });
  document.addEventListener("focusout", (event) => {
    if (event.target.closest(".chart-point[data-tooltip]")) hideTooltip();
  });
  document.addEventListener("htmx:beforeSwap", hideTooltip);
})();
