import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import {
  tmuxCommand,
  tmuxListCommand,
  validateTmuxPath,
  tmuxRenameCommand,
} from "../shared/tmux-command.js";

test("tmux launch resolves SSH PATH and preserves real tmux failures", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-tmux-"));
  try {
    writeFileSync(
      path.join(dir, "tmux"),
      "#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf 'fixture: terminal initialization failed\\n' >&2\nexit 23\n",
      { mode: 0o700 },
    );
    const result = spawnSync("/bin/sh", ["-c", tmuxCommand("work")], {
      encoding: "utf8",
      env: { ...process.env, PATH: dir },
    });
    assert.equal(result.status, 23);
    assert.match(
      result.stdout,
      /^-u\n-T\nclipboard\nnew-session\n-A\n-s\nwork\n;/,
    );
    assert.match(result.stdout, /set-clipboard external/);
    assert.match(result.stderr, /terminal initialization failed/);
    assert.match(result.stderr, /exit status 23/);
    assert.doesNotMatch(result.stderr, /Install tmux/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
test("explicit tmux executable works outside PATH, including spaces and quotes, for launch and discovery", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-tmux-"));
  try {
    const binary = path.join(dir, "custom ' tmux");
    writeFileSync(binary, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n", {
      mode: 0o700,
    });
    const launch = spawnSync(
      "/bin/sh",
      ["-c", tmuxCommand("work-01", binary)],
      { encoding: "utf8" },
    );
    assert.equal(launch.status, 0);
    assert.match(
      launch.stdout,
      /^-u\n-T\nclipboard\nnew-session\n-A\n-s\nwork-01\n;/,
    );
    assert.match(launch.stdout, /set-option\nmouse\non/);
    const list = spawnSync("/bin/sh", ["-c", tmuxListCommand(binary)], {
      encoding: "utf8",
    });
    assert.equal(list.status, 0);
    assert.equal(
      list.stdout,
      "list-sessions\n-F\n#{session_name}|#{session_windows}|#{session_attached}|#{session_id}|#{session_created}\n",
    );
    const missing = spawnSync(
      "/bin/sh",
      ["-c", tmuxCommand("main", path.join(dir, "absent"))],
      { encoding: "utf8" },
    );
    assert.equal(missing.status, 127);
    assert.match(
      missing.stderr,
      /Configured tmux executable is not executable/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
test("session and executable validation prevent command injection", () => {
  for (const value of ["../tmux", "tmux", "/bin/tmux\nwhoami"])
    assert.throws(() => validateTmuxPath(value));
  assert.throws(() => tmuxCommand("main'; echo injected"));
  for (const name of ["", "a b", "a;b", "a'b", "a\nb", "x".repeat(65)]) {
    assert.throws(() => tmuxRenameCommand("main", name));
    assert.throws(() => tmuxRenameCommand(name, "new-name"));
  }
});
