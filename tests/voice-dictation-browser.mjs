import assert from "node:assert/strict";
// UI lifecycle uses deterministic worker replies; speech-browser.mjs separately runs real WASM inference.
export async function mockSpeechWorker(context) {
  await context.route("**/speech-worker-*.js", (r) =>
    r.fulfill({
      contentType: "text/javascript",
      body: `self.onmessage=({data})=>setTimeout(()=>self.postMessage({id:data.id,type:'result',value:data.type==='load'?true:'dictated first\\nsecond'}), data.type==='load'?1500:300);`,
    }),
  );
}
export async function exerciseVoiceDictation(page, getInput) {
  await page.evaluate(() => {
    const original = navigator.mediaDevices.getUserMedia.bind(
      navigator.mediaDevices,
    );
    window.__speechTracks = [];
    window.__speechRequests = 0;
    navigator.mediaDevices.getUserMedia = async (...args) => {
      window.__speechRequests++;
      if (window.__denySpeech)
        throw new DOMException("Denied", "NotAllowedError");
      const stream = await original(...args);
      window.__speechTracks.push(...stream.getTracks());
      return stream;
    };
  });
  const before = getInput();
  await page.keyboard.press("Alt+Space");
  await page.locator("#voice-dialog[open]").waitFor();
  await page.waitForFunction(
    () => !document.querySelector("#voice-stop").disabled,
  );
  assert.equal(await page.evaluate(() => window.__speechRequests), 1);
  await page.waitForFunction(() =>
    [...document.querySelectorAll(".voice-wave span")].some(
      (bar) => Number(bar.style.getPropertyValue("--level")) > 0,
    ),
  );
  await page.keyboard.press("Escape");
  await page.waitForFunction(
    () =>
      window.__speechTracks.length > 0 &&
      window.__speechTracks.every((t) => t.readyState === "ended"),
  );
  assert.equal(getInput(), before);
  await page.evaluate(() => (window.__denySpeech = true));
  // Mac Option may change e.key; physical KeyV must still open the popup.
  await page.evaluate(() =>
    document.activeElement.dispatchEvent(
      new KeyboardEvent("keydown", {
        code: "KeyV",
        key: "◊",
        altKey: true,
        shiftKey: true,
        bubbles: true,
      }),
    ),
  );
  await page.locator("#voice-dialog[open]").waitFor();
  await page.waitForFunction(() =>
    document.querySelector("#voice-status").textContent.includes("denied"),
  );
  await page.evaluate(() => (window.__denySpeech = false));
  await page.locator("#voice-record").click();
  await page.waitForFunction(
    () => !document.querySelector("#voice-stop").disabled,
  );
  await page.waitForTimeout(800);
  assert.equal(await page.locator("#voice-transcript").count(), 0);
  assert.equal(
    await page.locator(".voice-time").getAttribute("aria-valuemax"),
    "60",
  );
  assert.ok(
    await page
      .locator(".voice-time > span")
      .evaluate((el) => parseFloat(el.style.width) > 0),
  );
  assert.equal(
    await page
      .locator("#voice-stop")
      .evaluate((el) => getComputedStyle(el).justifyContent),
    "center",
  );
  assert.equal(await page.locator(".voice-hint").count(), 0);
  assert.equal(await page.locator("#voice-insert").count(), 0);
  assert.ok(await page.locator("#voice-record").isHidden());
  assert.equal(getInput(), before, "Recording must not send terminal input");
  await page.screenshot({ path: ".build/voice-live-preview.png" });
  await page.locator("#voice-stop").click();
  await page.waitForFunction(() => !document.querySelector("#voice-dialog"));
  assert.ok(
    await page.evaluate(() =>
      window.__speechTracks.every((t) => t.readyState === "ended"),
    ),
  );
  await page.waitForTimeout(200);
  assert.equal(getInput().slice(before.length), "dictated first second");

  console.log(
    "Minimal dictation, automatic recording, waveform, denial, cancellation and Stop insertion passed.",
  );
}
