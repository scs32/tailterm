import test from "node:test";
import assert from "node:assert/strict";
import { hasNewText } from "../client/activity.js";

const snapshot = (
  lines,
  { baseY = 0, cursorY = lines.length - 1, cursorX, cursorLine } = {},
) => ({
  lines,
  baseY,
  cursorY,
  cursorX: cursorX ?? (cursorLine ?? lines.at(-1) ?? "").length,
  cursorLine: cursorLine ?? lines.at(-1) ?? "",
});

test("activity suppresses in-place redraws without hiding real output", () => {
  assert.equal(
    hasNewText(
      snapshot(["build 41%", "prompt"], {
        cursorY: 0,
        cursorX: 9,
        cursorLine: "build 41%",
      }),
      snapshot(["build 42%", "prompt"], {
        cursorY: 0,
        cursorX: 9,
        cursorLine: "build 42%",
      }),
    ),
    false,
    "a progress/counter rewrite is not new output",
  );
  assert.equal(
    hasNewText(
      snapshot(["working /", "details", "prompt"], { cursorY: 2 }),
      snapshot(["working -", "details", "prompt"], { cursorY: 2 }),
    ),
    false,
    "a fixed-row spinner repaint is not new output",
  );
  assert.equal(
    hasNewText(
      snapshot(["prompt", "partial"], {
        cursorY: 1,
        cursorX: 7,
        cursorLine: "partial",
      }),
      snapshot(["prompt", "partial response"], {
        cursorY: 1,
        cursorX: 16,
        cursorLine: "partial response",
      }),
    ),
    true,
    "text appended at the cursor remains genuine output",
  );
  assert.equal(
    hasNewText(
      snapshot(["prompt", "first"], { cursorY: 1 }),
      snapshot(["prompt", "first", "second"], { cursorY: 2 }),
    ),
    true,
    "output advancing into a new row remains genuine",
  );
  assert.equal(
    hasNewText(
      snapshot(["first", "second", "third"], {
        baseY: 12,
        cursorY: 2,
      }),
      snapshot(["second", "third", "fourth"], {
        baseY: 13,
        cursorY: 2,
      }),
    ),
    true,
    "output scrolling the terminal remains genuine",
  );
  assert.equal(
    hasNewText(
      snapshot(["first", "second", "prompt"], {
        baseY: 12,
        cursorY: 2,
      }),
      snapshot(["second", "third", "prompt"], {
        baseY: 12,
        cursorY: 2,
      }),
    ),
    true,
    "tmux upward scrolling remains genuine without xterm position changes",
  );
});
