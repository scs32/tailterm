import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";
const html = `<html><head><link rel="stylesheet" href="/client/style.css"></head><body><dialog open><div id="picker"></div></dialog><script type="module">
import {projectFolderHTML,wireProjectFolder} from '/client/project-folder.js';
const qa=window.qa={server:{id:'a',name:'Mini',host:'mini'},closed:0,delay:false};
document.querySelector('#picker').innerHTML=projectFolderHTML('project');
const host={openSFTP:async()=>{if(qa.delay)await new Promise(r=>qa.release=r);return {done:new Promise(()=>{}),home:async()=>'/home/test',realpath:async p=>p,list:async p=>{if(p==='/missing')throw Error('Folder does not exist');return {path:p,entries:p==='/home/test'?[{name:'project with spaces',isDir:true},{name:'secret.txt',isDir:false}]:[]}},close(){qa.closed++}}}};
wireProjectFolder(document.querySelector('.project-folder'),host,()=>qa.server);
</script></body></html>`;
const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "picker",
      configureServer(s) {
        s.middlewares.use("/picker", (req, res) => {
          res.setHeader("content-type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
try {
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 390, height: 650 },
      });
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/picker`,
      );
      await page.locator("[data-folder-browse]").click();
      await page.locator('[data-folder-entry="0"]').click();
      await page.locator("[data-folder-use]").click();
      assert.equal(
        await page.locator("#project").inputValue(),
        "/home/test/project with spaces",
      );
      assert.equal(await page.evaluate(() => qa.closed), 2);
      await page.locator("#project").fill("/missing");
      await page.locator("[data-folder-browse]").click();
      await page
        .locator(".project-folder-browser")
        .filter({ hasText: "Folder does not exist" })
        .waitFor();
      await page.locator("[data-folder-browse]").click();
      await page.locator("#project").fill("");
      await page.evaluate(() => (qa.delay = true));
      await page.locator("[data-folder-browse]").click();
      await page.waitForFunction(() => qa.release);
      await page.evaluate(() => {
        qa.server = { id: "b", name: "Air", host: "air" };
        qa.release();
      });
      await page.waitForFunction(() => qa.closed === 4);
      assert.equal(
        await page.locator("[data-folder-use]").count(),
        0,
        "stale machine offered a folder",
      );
      assert.equal(await page.locator("#project").inputValue(), "");
      await page.locator("[data-folder-browse]").click();
      await page.evaluate(() => (qa.delay = false));
      await page.locator("[data-folder-browse]").click();
      await page.locator("[data-folder-use]").waitFor();
      assert.match(await page.locator(".folder-current").textContent(), /Air/);
      assert.equal(
        await page.locator(".folder-directories").textContent(),
        "project with spaces/",
      );
      assert.ok(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      );
      await page.screenshot({ path: ".build/project-picker-" + name + ".png" });
      console.log(
        name +
          ": directory navigation, spaced path, closed SFTP connections, errors, stale machine rejection, mobile bounds passed.",
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
