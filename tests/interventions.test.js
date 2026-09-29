import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";
import {
  INTERVENTION_KINDS,
  formatKindCounts,
  interventionHeadline,
  interventionsHtml,
  kindCounts,
} from "../client/interventions-format.js";

const summary = {
  timeZone: "America/Los_Angeles",
  total: 3,
  linked: 2,
  byKind: { release: 1, nudge: 2 },
  days: [
    {
      day: "2026-09-28",
      total: 2,
      linked: 1,
      byKind: { nudge: 1, release: 1 },
    },
    { day: "2026-09-27", total: 1, linked: 1, byKind: { nudge: 1 } },
  ],
  unlinked: [
    {
      seq: 42,
      kind: "release",
      itemId: "wi_1111111111111111",
      day: "2026-09-28",
    },
  ],
};

test("Interventions client reads the project route with its time zone", async () => {
  const calls = [];
  const client = createHubClient({
    baseURL: "https://hub.example",
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return { status: 200, text: async () => "{}" };
    },
  });
  await client.listInterventions("tsk_fixture", {
    tz: "America/Los_Angeles",
    limit: 1,
  });
  assert.deepEqual(
    calls.map(({ url, init }) => [url, init.method]),
    [
      [
        "https://hub.example/v1/tasks/tsk_fixture/interventions?tz=America%2FLos_Angeles&limit=1",
        "GET",
      ],
    ],
  );
});

test("Kinds match the hub vocabulary and sort in its order", () => {
  assert.deepEqual(INTERVENTION_KINDS, [
    "release",
    "nudge",
    "decision-on-behalf",
    "gate-fix",
    "cleanup",
    "diagnosis",
    "status",
    "other",
  ]);
  assert.deepEqual(
    kindCounts({ other: 1, "gate-fix": 3, future: 2, nudge: 0 }),
    [
      { kind: "gate-fix", count: 3 },
      { kind: "other", count: 1 },
      { kind: "future", count: 2 },
    ],
  );
  assert.equal(
    formatKindCounts({ nudge: 2, release: 1 }),
    "release 1 · nudge 2",
  );
});

test("Headline counts interventions and product-item links", () => {
  assert.equal(interventionHeadline(null), "No interventions recorded");
  assert.equal(
    interventionHeadline({ total: 0, linked: 0 }),
    "No interventions recorded",
  );
  assert.equal(
    interventionHeadline({ total: 1, linked: 0 }),
    "1 intervention · 0 of 1 linked to a product item",
  );
  assert.equal(
    interventionHeadline(summary),
    "3 interventions · 2 of 3 linked to a product item",
  );
});

test("Summary renders per-day rows, per-kind totals and identifiable unlinked records", () => {
  const html = interventionsHtml(summary);
  assert.match(html, /3 interventions · 2 of 3 linked to a product item/);
  assert.match(html, /days in America\/Los_Angeles/);
  assert.match(
    html,
    /<tr><td>release<\/td><td>1<\/td><\/tr><tr><td>nudge<\/td><td>2<\/td><\/tr>/,
  );
  assert.match(
    html,
    /data-intervention-day="2026-09-28"><td>2026-09-28<\/td><td>2<\/td><td>1<\/td><td>release 1 · nudge 1<\/td>/,
  );
  assert.match(
    html,
    /data-intervention-day="2026-09-27"><td>2026-09-27<\/td><td>1<\/td><td>1<\/td><td>nudge 1<\/td>/,
  );
  assert.match(
    html,
    /data-intervention-unlinked="42">#42 · release · wi_1111111111111111 · 2026-09-28</,
  );
  assert.doesNotMatch(html, /older not shown/);
  assert.equal(
    interventionsHtml({ total: 0, linked: 0, timeZone: "UTC" }).includes(
      "<table",
    ),
    false,
  );
});

test("Summary escapes hub text and notes unlinked records beyond the list", () => {
  const html = interventionsHtml({
    ...summary,
    total: 5,
    linked: 2,
    byKind: { "<b>x</b>": 5 },
    unlinked: [
      { seq: 1, kind: "<img src=x>", itemId: "wi_<script>", day: "d" },
    ],
  });
  assert.equal(/<b>|<img|<script/.test(html), false);
  assert.match(html, /2 older not shown/);
});
