export function showCommandPalette({ dialog, close, commands }) {
  dialog(
    "Commands",
    '<input id="command-query" aria-label="Find a session or action" autocomplete="off" placeholder="Search sessions and commands..."><div id="command-results"></div><p class="fine">Arrow keys to choose · Enter to run · Escape to close</p>',
  );
  const query = document.querySelector("#command-query"),
    results = document.querySelector("#command-results");
  let index = 0,
    matches = [];
  const draw = () => {
    matches = commands.filter((c) =>
      c.label.toLowerCase().includes(query.value.toLowerCase()),
    );
    index = Math.min(index, Math.max(0, matches.length - 1));
    results.replaceChildren();
    matches.forEach((command, i) => {
      const b = document.createElement("button");
      b.textContent = command.label;
      b.classList.toggle("selected", i === index);
      b.onclick = () => {
        close();
        command.run();
      };
      results.append(b);
    });
    if (!matches.length)
      results.textContent = "No matching sessions or actions.";
  };
  query.oninput = () => {
    index = 0;
    draw();
  };
  query.onkeydown = (e) => {
    if (["ArrowDown", "ArrowUp"].includes(e.key)) {
      e.preventDefault();
      index = Math.max(
        0,
        Math.min(matches.length - 1, index + (e.key === "ArrowDown" ? 1 : -1)),
      );
      draw();
      results.children[index]?.scrollIntoView({ block: "nearest" });
    }
    if (e.key === "Enter" && matches[index]) {
      e.preventDefault();
      close();
      matches[index].run();
    }
  };
  draw();
  query.focus();
}
