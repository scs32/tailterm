export async function assertTerminalBounds(page) {
  await page.waitForFunction(
    () => {
      const panes = [
        ...document.querySelectorAll(".terminal-instance:not([hidden])"),
      ];
      return (
        panes.length > 0 &&
        panes.every((pane) => {
          const screen = pane.querySelector(".xterm-screen");
          if (!screen) return false;
          const bounds = pane.getBoundingClientRect();
          const rendered = screen.getBoundingClientRect();
          const style = getComputedStyle(pane);
          return (
            rendered.height > 0 &&
            rendered.bottom <=
              bounds.bottom -
                parseFloat(style.paddingBottom) -
                parseFloat(style.borderBottomWidth) +
                0.5 &&
            rendered.right <=
              bounds.right -
                parseFloat(style.paddingRight) -
                parseFloat(style.borderRightWidth) +
                0.5
          );
        })
      );
    },
    null,
    { timeout: 5000 },
  );
}
