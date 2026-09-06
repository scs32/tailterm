import test from "node:test";
import assert from "node:assert/strict";
import { historyText } from "../client/history-format.js";

test("history keeps Unicode, line breaks and SGR colors/attributes", () => {
  const text =
    "❯ hello\n\t\x1b[1;3;4;7mstyled\x1b[0m\x1b[38;2;255;80;20mRGB\x1b[48;5;23m indexed\x1b[0m";
  assert.equal(historyText(text), text);
  assert.equal(
    historyText("\x9b38:2::10:20:30mcolor"),
    "\x1b[38:2::10:20:30mcolor",
  );
});
test("history strips terminal controls, clipboard/title requests and control strings", () => {
  const text =
    "before\x1b]52;c;c2VjcmV0\x07\x1b]0;new title\x1b\\\x1bPsecret\x1b\\\x1b[?1049h\x1b[2J\x1b[6n\x1b[?1000h\x07\rafter";
  assert.equal(historyText(text), "beforeafter");
  assert.equal(historyText("before\x9d52;c;payload\x9cafter"), "beforeafter");
  assert.equal(historyText("before\x1b]unfinished"), "before");
  assert.equal(historyText("\x1b[31mred\x1b[0m\x1b[2J"), "\x1b[31mred\x1b[0m");
});
