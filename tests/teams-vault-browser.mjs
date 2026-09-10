// Real Agents/Teams editors + encrypted vault in isolated Chromium/WebKit contexts.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/client/style.css"></head><body><div id="workspace"><aside><div class="brand"><strong>tailterm</strong></div><div class="sidebar-section"><span>SERVERS</span><button id="all-servers" aria-pressed="true">All</button></div><input id="filter" class="filter" placeholder="Find a server"><nav id="server-list"><button class="server-item selected" aria-pressed="true"><span class="node-icon">◇</span><div><strong>Synthetic machine with a realistically long display name</strong><small>Fixture · SSH</small></div><span class="node-dot online"></span></button></nav><div class="sidebar-bottom"><button>SSH keys</button><button>Lock vault</button></div></aside><main><header><div class="header-right"><button><span class="status-dot"></span><span>Connected test profile</span></button></div></header><section class="terminal-shell"><div class="terminal-tabs"><div class="tab-strip"></div><div class="terminal-tools"><button>Find</button><button>Appearance</button></div></div><div id="terminal-body"></div><div class="terminal-footer"><span>Fixture terminal</span></div></section></main></div><dialog id="dialog"></dialog><p id="notice"></p><script type="module">
import * as vault from '/client/local-vault.js';
import {createAgentsView} from '/client/agents-view.js';
import {createTeamsView} from '/client/teams-view.js';
import {setupModes} from '/client/modes.js';
await vault.localAPI('/unlock','POST',{password:'isolated agent library passphrase'});
if(!vault.localData().servers.length)await vault.localAPI('/servers','POST',{name:'Synthetic machine with a realistically long display name',host:'test.example',port:22,username:'test',mode:'ssh'});
const host={getData:()=>vault.localData(),getServers:()=>vault.localData().servers,currentServer:()=>vault.localData().servers[0],api:vault.localAPI,reloadData:async()=>{},notice:t=>document.querySelector('#notice').textContent=t,confirm:async()=>true,inspectTools:async()=>({runtime:'codex',version:'fixture'}),newTask(){},addTeam(){},openAgents(){modes.set('agents')},dialog(title,body){const d=document.querySelector('#dialog');d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2></div>'+body;d.showModal()},closeDialog(){document.querySelector('#dialog').close()}};
let agents,teams;
const modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),available:mode=>['terminals','agents','teams'].includes(mode),onChange:(mode,view)=>{agents.hide();teams.hide();view.replaceChildren();const active=mode==='agents'?agents:teams;active.mount(view);active.show()}});
agents=createAgentsView(host);teams=createTeamsView(host);modes.refresh();modes.set('agents');window.qa={vault,modes,show:mode=>modes.set(mode),refresh:()=>{agents.refresh();teams.refresh()}};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "agents-library-check",
      configureServer(instance) {
        instance.middlewares.use("/agents-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const url = `http://127.0.0.1:${server.httpServer.address().port}/agents-test`;
try {
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({
        viewport: { width: 1120, height: 820 },
      });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(url);
      await page.locator("#agents-new").click();
      await page
        .locator('[data-field="name"]')
        .fill("Implementation agent with long reusable context");
      await page.locator('[data-field="launchName"]').fill("builder");
      await page.locator('[data-field="role"]').fill("Builder");
      await page
        .locator("#agent-library-model-choice")
        .selectOption("gpt-6-astra");
      await page.locator(".agent-controls > summary").click();
      await page.locator('[data-field="reasoning"]').selectOption("high");
      await page
        .locator('[data-field="approvalMode"]')
        .selectOption("on-request");
      await page
        .locator('[data-field="sandboxMode"]')
        .selectOption("workspace-write");
      await page
        .locator("#agent-library-cwd")
        .fill("/synthetic/project with spaces");
      await page
        .locator('[data-field="prompt"]')
        .fill(
          "Review exact work-item identity and preserve partial launch state.\n" +
            "Long content remains readable without overlapping controls. ".repeat(
              60,
            ),
        );
      await page.locator('#agent-library-form button[type="submit"]').click();
      await page.waitForTimeout(250);
      assert.equal(
        await page
          .locator("#agent-library-error")
          .textContent()
          .catch(() => ""),
        "",
      );
      await page
        .locator(".agents-detail h2")
        .filter({ hasText: "Implementation agent" })
        .waitFor({ timeout: 5000 });
      const stored = await page.evaluate(() => qa.vault.localData());
      assert.equal(stored.agentCatalog.version, 2);
      assert.equal(stored.agentCatalog.definitions[0].reasoning, "high");
      assert.equal(stored.teamsVersion, 2);
      assert.doesNotMatch(JSON.stringify(stored.teams), /gpt-6-astra/);
      await page.evaluate(async () => {
        const original = qa.vault.localData().agentCatalog.definitions[0];
        await qa.vault.localAPI("/agents", "POST", {
          ...original,
          id: "agent_layout_second",
          revision: 1,
          name: "Second reusable agent with another long display name",
          launchName: "layout-reviewer",
          role: "Reviewer",
          prompt:
            "A second selectable definition for layout interaction checks.",
        });
        qa.refresh();
      });
      await page.evaluate(() => qa.show("teams"));
      await page.locator("#teams-new").click();
      await page.locator("#team-name").fill("Long-content delivery team");
      await page.locator('[data-field="alias"]').fill("lead-builder");
      await page.locator('[data-field="role"]').fill("Orchestrator");
      await page.locator('#team-form button[type="submit"]').click();
      await page
        .locator(".teams-detail h2")
        .filter({ hasText: "Long-content delivery team" })
        .waitFor();
      const finalData = await page.evaluate(() => qa.vault.localData());
      assert.equal(
        finalData.teams[0].members[0].agentDefinitionId,
        finalData.agentCatalog.definitions[0].id,
      );
      assert.equal(finalData.teams[0].members[0].alias, "lead-builder");
      await page.evaluate(async () => {
        const current = qa.vault.localData().teams[0];
        const members = Array.from({ length: 32 }, (_, index) => ({
          agentDefinitionId: current.members[0].agentDefinitionId,
          alias: `layout-member-${String(index + 1).padStart(2, "0")}`,
          role: (
            `Long layout role ${index + 1} keeps realistic team content wrapping ` +
            "without hiding actions or detail scrolling controls"
          ).slice(0, 80),
        }));
        const expanded = {
          ...current,
          members,
          orchestrator: members[0].alias,
        };
        await qa.vault.localAPI("/teams", "POST", expanded);
        await qa.vault.localAPI("/teams", "POST", {
          ...current,
          id: "team_layout_second",
          name: "Second selectable team with a long rail label",
          members: [
            {
              agentDefinitionId: current.members[0].agentDefinitionId,
              alias: "second-team-lead",
              role: "Orchestrator",
            },
          ],
          orchestrator: "second-team-lead",
        });
        qa.refresh();
      });
      const fences = await page.evaluate(async () => {
        const original = qa.vault.localData().agentCatalog.definitions[0];
        await qa.vault.localAPI("/agents", "POST", {
          ...original,
          prompt: `A central edit for future launches.\n${original.prompt}`,
        });
        let stale = "",
          referenced = "";
        try {
          await qa.vault.localAPI("/agents", "POST", {
            ...original,
            prompt: "stale overwrite",
          });
        } catch (error) {
          stale = error.message;
        }
        try {
          await qa.vault.localAPI("/agents/" + original.id, "DELETE");
        } catch (error) {
          referenced = error.message;
        }
        return {
          stale,
          referenced,
          current: qa.vault
            .localData()
            .agentCatalog.definitions.find(
              (definition) => definition.id === original.id,
            ),
        };
      });
      assert.match(fences.stale, /changed in another editor/);
      assert.match(fences.referenced, /used by teams/);
      assert.equal(fences.current.revision, 2);
      for (const mode of ["agents", "teams"])
        for (const width of [320, 390, 680, 760, 761, 1120]) {
          await page.setViewportSize({
            width,
            height: width <= 760 ? 640 : 760,
          });
          await page.evaluate((nextMode) => qa.show(nextMode), mode);
          const root = page.locator(`.${mode}-view`);
          await root.waitFor();
          const choices = root.locator(".board-rail [data-board-task]");
          assert.equal(await choices.count(), 2);
          const titles = await choices.evaluateAll((buttons) =>
              buttons.map((button) => button.title),
            ),
            selectedIndex = titles.findIndex((title) =>
              title.startsWith(
                mode === "agents"
                  ? "Implementation agent"
                  : "Long-content delivery team",
              ),
            ),
            otherIndex = selectedIndex === 0 ? 1 : 0;
          assert.notEqual(selectedIndex, -1);
          await choices.nth(otherIndex).click();
          await root
            .locator(".board-thread h2")
            .filter({ hasText: titles[otherIndex] })
            .waitFor();
          await root
            .locator(".board-rail [data-board-task]")
            .nth(selectedIndex)
            .click();
          await root
            .locator(".board-thread h2")
            .filter({ hasText: titles[selectedIndex] })
            .waitFor();
          await page.mouse.move(0, 0);
          await page.evaluate(() => document.activeElement?.blur());

          const geometry = await page.evaluate((activeMode) => {
            const rect = (element) => {
              const value = element.getBoundingClientRect();
              return {
                left: value.left,
                right: value.right,
                top: value.top,
                bottom: value.bottom,
                width: value.width,
                height: value.height,
              };
            };
            const view = document.querySelector("#mode-view"),
              root = document.querySelector(`.${activeMode}-view`),
              rail = root.querySelector(".board-rail"),
              thread = root.querySelector(".board-thread"),
              header = document.querySelector("#workspace > main > header"),
              switcher = header.querySelector(".mode-switch"),
              selected = rail.querySelector('[aria-pressed="true"]'),
              other = rail.querySelector('[aria-pressed="false"]');
            thread.scrollTop = 0;
            return {
              viewport: { width: innerWidth, height: innerHeight },
              documentWidth: document.documentElement.scrollWidth,
              view: rect(view),
              root: rect(root),
              rail: rect(rail),
              thread: {
                ...rect(thread),
                clientHeight: thread.clientHeight,
                scrollHeight: thread.scrollHeight,
              },
              header: rect(header),
              switcher: rect(switcher),
              actions: [...root.querySelectorAll(".view-actions button")].map(
                rect,
              ),
              selectedBackground: getComputedStyle(selected).backgroundColor,
              otherBackground: getComputedStyle(other).backgroundColor,
              otherBorder: getComputedStyle(other).borderColor,
            };
          }, mode);
          assert.ok(
            geometry.documentWidth <= geometry.viewport.width + 1,
            JSON.stringify({ mode, width, geometry }),
          );
          assert.ok(
            geometry.view.height > 100 &&
              geometry.view.top >= geometry.header.bottom - 1 &&
              geometry.view.bottom <= geometry.viewport.height + 1,
            JSON.stringify({ mode, width, geometry }),
          );
          assert.ok(
            geometry.switcher.left >= -1 &&
              geometry.switcher.bottom <= geometry.header.bottom + 1,
            JSON.stringify({ mode, width, geometry }),
          );
          assert.ok(
            geometry.root.left >= geometry.view.left - 1 &&
              geometry.root.right <= geometry.view.right + 1 &&
              geometry.root.top >= geometry.view.top - 1 &&
              geometry.root.bottom <= geometry.view.bottom + 1 &&
              geometry.thread.clientHeight > 0,
            JSON.stringify({ mode, width, geometry }),
          );
          assert.ok(
            geometry.rail.left >= geometry.view.left - 1 &&
              geometry.thread.right <= geometry.view.right + 1,
            JSON.stringify({ mode, width, geometry }),
          );
          if (width <= 760)
            assert.ok(
              geometry.rail.bottom <= geometry.thread.top + 1,
              `narrow rail overlaps ${mode} detail at ${width}px`,
            );
          else
            assert.ok(
              geometry.rail.right <= geometry.thread.left + 1,
              `desktop rail overlaps ${mode} detail at ${width}px`,
            );
          for (const action of geometry.actions)
            assert.ok(
              action.left >= geometry.thread.left - 1 &&
                action.right <= geometry.thread.right + 1 &&
                action.top >= geometry.thread.top - 1 &&
                action.bottom <= geometry.thread.bottom + 1 &&
                action.bottom <= geometry.viewport.height + 1,
              JSON.stringify({ mode, width, action, geometry }),
            );
          for (let i = 0; i < geometry.actions.length; i++)
            for (let j = i + 1; j < geometry.actions.length; j++) {
              const a = geometry.actions[i],
                b = geometry.actions[j];
              assert.ok(
                a.right <= b.left ||
                  b.right <= a.left ||
                  a.bottom <= b.top ||
                  b.bottom <= a.top,
                `${mode} action controls overlap at ${width}px`,
              );
            }
          assert.notEqual(
            geometry.selectedBackground,
            geometry.otherBackground,
            `${mode} selection is not rendered as a fill at ${width}px`,
          );

          const currentChoices = root.locator(".board-rail [data-board-task]");
          const other = currentChoices.nth(otherIndex);
          await other.hover();
          const hover = await other.evaluate((button) => ({
            background: getComputedStyle(button).backgroundColor,
            border: getComputedStyle(button).borderColor,
          }));
          assert.notEqual(
            hover.background,
            geometry.selectedBackground,
            `${mode} hover used the selected fill at ${width}px`,
          );
          assert.notEqual(
            hover.border,
            geometry.otherBorder,
            `${mode} hover has no outline at ${width}px`,
          );
          await page.keyboard.press("Tab");
          await other.focus();
          const focus = await other.evaluate((button) => ({
            active: document.activeElement === button,
            visible: button.matches(":focus-visible"),
            background: getComputedStyle(button).backgroundColor,
            border: getComputedStyle(button).borderColor,
          }));
          assert.equal(focus.active, true);
          assert.equal(focus.visible, true);
          assert.notEqual(focus.background, geometry.selectedBackground);
          assert.notEqual(focus.border, geometry.otherBorder);

          const scrolled = await page.evaluate((activeMode) => {
            const thread = document.querySelector(
                `.${activeMode}-view > .board-thread`,
              ),
              last =
                activeMode === "agents"
                  ? thread.querySelector(
                      ".agent-detail-list .team-row:last-child",
                    )
                  : thread.querySelector(".team-list .team-row:last-child");
            thread.scrollTop = thread.scrollHeight;
            const threadRect = thread.getBoundingClientRect(),
              lastRect = last.getBoundingClientRect();
            const result = {
              scrollTop: thread.scrollTop,
              scrollHeight: thread.scrollHeight,
              clientHeight: thread.clientHeight,
              lastBottom: lastRect.bottom,
              threadBottom: threadRect.bottom,
              viewportHeight: innerHeight,
            };
            thread.scrollTop = 0;
            return result;
          }, mode);
          assert.ok(
            scrolled.scrollHeight > scrolled.clientHeight &&
              scrolled.scrollTop > 0 &&
              scrolled.lastBottom <= scrolled.threadBottom + 1 &&
              scrolled.lastBottom <= scrolled.viewportHeight + 1,
            JSON.stringify({ mode, width, scrolled }),
          );
        }
      assert.deepEqual(errors, []);
      await context.close();
      console.log(
        `${name}: Agents and Teams production chrome, 320/390/680/760/761/1120 geometry, long-content scrolling, native interaction, selection fill, and hover/focus outlines passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
