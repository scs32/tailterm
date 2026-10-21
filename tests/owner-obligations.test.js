import test from "node:test";
import assert from "node:assert/strict";
import {
  displayText,
  ownerWaitSummary,
  ownerAge,
  renderOwnerRequests,
} from "../client/owner-obligations.js";
import { renderTeamDelivery } from "../client/team-delivery-view.js";
const request = (id, item, created = "2026-09-26T00:00:00Z") => ({
  id,
  taskId: "task",
  recipientKind: "owner",
  state: "delivered",
  createdAt: created,
  dueAt: "2026-09-26T00:30:00Z",
  subject: "Please approve",
  messageSeq: 3,
  request: {
    text: "Full 矩阵 <request>",
    envelope: { expectedAnswer: "APPROVE exact\n" },
    workItems: [{ itemTaskId: "task", itemId: item, relationship: "primary" }],
  },
});
test("owner wait isolates exact project/item and oldest open request", () => {
  const rows = [
    request("a", "one"),
    request("b", "one", "2026-09-26T00:01:00Z"),
    request("c", "two"),
    { ...request("d", "one"), state: "closed" },
  ];
  assert.match(
    ownerWaitSummary(rows, "task", "one", Date.parse("2026-09-26T00:02:01Z")),
    /2 requests.*2m 1s/,
  );
  assert.match(ownerWaitSummary(rows, "task", "two"), /1 request/);
  assert.equal(ownerWaitSummary(rows, "other", "one"), "");
  assert.equal(ownerWaitSummary(rows, "task", "none"), "");
});
test("owner panel escapes and retains full request and exact approval affordance", () => {
  const html = renderOwnerRequests([request("a", "one")]);
  assert.match(html, /Full 矩阵 &lt;request&gt;/);
  assert.match(html, /data-owner-approve="a"/);
  assert.equal(
    renderOwnerRequests([{ ...request("a", "one"), state: "closed" }]),
    "",
  );
  assert.equal(ownerAge(0, 125000), "2m 5s");
});
test("delivery uses live item-bound owner summary without altering queue state", () => {
  const html = renderTeamDelivery(
    {
      entries: [
        { itemId: "one", state: "running" },
        { itemId: "two", state: "running" },
      ],
    },
    [],
    [request("a", "one")],
    "task",
  );
  assert.equal((html.match(/Waiting on owner/g) || []).length, 1);
  assert.match(html, /running/);
});
test("owner panel shows typed body fields with their line breaks by the Board rule", () => {
  const ask = "Choose one:\n  - keep <it>\n  - drop it";
  const body = "First line.\nSecond line.";
  const typed = {
    ...request("t", "one"),
    request: {
      text: `REQUEST: Pick a path\nTo: owner\nAsk: Choose one: - keep <it> - drop it\nText: First line. Second line.\nExpected answer:\nAPPROVE keep\nnow`,
      envelope: {
        kind: "request",
        subject: "Pick a path",
        body: { ask, text: body },
        expectedAnswer: "APPROVE keep\nnow",
      },
    },
  };
  const html = renderOwnerRequests([typed]);
  assert.ok(
    html.includes(
      "Ask:\nChoose one:\n  - keep &lt;it&gt;\n  - drop it\nText:\nFirst line.\nSecond line.\nExpected answer:\nAPPROVE keep\nnow</pre>",
    ),
    html,
  );
  assert.ok(
    html.includes(
      `<pre style="white-space:pre-wrap;overflow-wrap:anywhere">REQUEST: Pick a path\nTo: owner\nAsk:\n`,
    ),
  );
  assert.ok(!html.includes("Ask: Choose one: - keep"));
  // One shared rule: the panel shows exactly what the Board list shows.
  assert.ok(
    html.includes(
      displayText(typed.request)
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;"),
    ),
  );
  assert.match(html, /data-owner-approve="t"/);
  const untyped = {
    ...request("u", "one"),
    request: { text: "Plain <ask>\nAsk: a b\n  kept as stored" },
  };
  assert.ok(
    renderOwnerRequests([untyped]).includes(
      ">Plain &lt;ask&gt;\nAsk: a b\n  kept as stored</pre>",
    ),
  );
  assert.doesNotMatch(renderOwnerRequests([untyped]), /data-owner-approve/);
});
