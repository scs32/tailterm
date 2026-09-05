const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
let queue = Promise.resolve();
let importKey;
export function setKeyImporter(fn) {
  importKey = fn;
}
export function loginPrompt(prompt, signal) {
  const pending = queue.then(
    () =>
      new Promise((resolve) => {
        if (signal.aborted) {
          resolve({ cancel: true });
          return;
        }
        const d = document.createElement("dialog");
        d.className = "login-prompt";
        const finish = (answer) => {
          signal.removeEventListener("abort", abort);
          d.close();
          d.remove();
          resolve(answer);
        };
        const abort = () => finish({ cancel: true });
        signal.addEventListener("abort", abort, { once: true });
        const title =
          prompt.kind === "host-key"
            ? "Trust this SSH server?"
            : prompt.kind === "keyboard"
              ? "SSH verification"
              : "Sign in to SSH";
        let fields = "";
        if (prompt.kind === "host-key")
          fields = `<p>First connection to <strong>${esc(prompt.host)}:${prompt.port}</strong>.</p><code class="host-fingerprint">${esc(prompt.fingerprint)}</code><p class="fine">Compare this fingerprint with the server’s trusted console. Once accepted, Tailterm remembers it and rejects unexpected changes.</p><button class="primary" type="submit">Trust & continue</button>`;
        else if (prompt.kind === "keyboard")
          fields = `${prompt.host ? `<p><strong>${esc(prompt.username)}@${esc(prompt.host)}</strong></p>` : ""}<p>${esc(prompt.name)}</p><p>${esc(prompt.instructions)}</p>${prompt.prompts.map((p, i) => `<label>${esc(p.prompt)}<input name="answer-${i}" type="${p.echo ? "text" : "password"}" autocomplete="off" maxlength="4096"></label>`).join("")}<button class="primary" type="submit">Continue</button>`;
        else
          fields = `<p><strong>${esc(prompt.username)}@${esc(prompt.host)}</strong></p>${prompt.retry ? '<p class="fine">The previous credential was not accepted, or another authentication step is required.</p>' : ""}<label>Sign in with<select name="method">${prompt.methods.includes("publickey") ? '<option value="publickey">SSH key</option>' : ""}${prompt.methods.includes("password") ? '<option value="password">Password</option>' : ""}${prompt.methods.includes("keyboard-interactive") ? '<option value="keyboard-interactive">Interactive login / verification code</option>' : ""}</select></label><div data-key><label>Saved key<select name="keyId">${prompt.keys.map((k) => `<option value="${esc(k.id)}">${esc(k.name)}</option>`).join("")}</select></label><p class="fine">Or import a key now:</p><label>Key file<input name="keyFile" type="file"></label><label>Key passphrase (if encrypted)<input name="keyPassphrase" type="password" autocomplete="off"></label></div><div data-password><label>Password<input name="password" type="password" autocomplete="current-password" maxlength="4096"></label><label class="remember-password"><input name="remember" type="checkbox"> Remember in encrypted vault</label></div><p class="prompt-error" role="alert"></p><button class="primary" type="submit">Sign in</button>`;
        fields = fields
          .replace(
            '<button class="primary" type="submit">',
            '<div class="dialog-actions"><button class="primary" type="submit">',
          )
          .replace("</button>", "</button></div>");
        d.setAttribute("aria-labelledby", "login-prompt-title");
        d.innerHTML = `<div class="dialog-head"><h2 id="login-prompt-title">${title}</h2><button type="button" data-cancel aria-label="Cancel login">×</button></div><form>${fields}</form>`;
        (document.fullscreenElement || document.body).append(d);
        d.showModal();
        d.querySelector("[data-cancel]").onclick = abort;
        d.oncancel = (e) => {
          e.preventDefault();
          abort();
        };
        const form = d.querySelector("form");
        if (prompt.kind === "credentials") {
          if (!prompt.keys.length && prompt.methods.includes("password"))
            form.elements.method.value = "password";
          const toggle = () => {
            d.querySelector("[data-key]").hidden =
              form.elements.method.value !== "publickey";
            d.querySelector("[data-password]").hidden =
              form.elements.method.value !== "password";
          };
          form.elements.method.onchange = toggle;
          toggle();
        }
        if (prompt.kind === "keyboard") form.querySelector("input")?.focus();
        if (prompt.kind === "credentials") {
          const method = form.elements.method.value;
          (method === "password"
            ? form.elements.password
            : method === "publickey"
              ? prompt.keys.length
                ? form.elements.keyId
                : form.elements.keyFile
              : form.elements.method
          )?.focus();
        }
        form.onsubmit = async (e) => {
          e.preventDefault();
          if (prompt.kind === "host-key") {
            finish({ accept: true });
            return;
          }
          if (prompt.kind === "keyboard") {
            finish({
              answers: prompt.prompts.map(
                (_, i) => form.elements["answer-" + i].value,
              ),
            });
            return;
          }
          const button = form.querySelector("[type=submit]");
          button.disabled = true;
          try {
            const method = form.elements.method.value;
            let keyId = form.elements.keyId.value;
            if (method === "publickey" && form.elements.keyFile.files[0]) {
              const file = form.elements.keyFile.files[0];
              if (file.size > 64000) throw new Error("Key file is too large.");
              const body = {
                name: file.name,
                privateKey: await file.text(),
                passphrase: form.elements.keyPassphrase.value,
              };
              let result;
              if (importKey) result = await importKey(body);
              else {
                const r = await fetch("/api/keys", {
                  method: "POST",
                  headers: { "Content-Type": "application/json" },
                  signal,
                  body: JSON.stringify(body),
                });
                result = await r.json();
                if (!r.ok) throw new Error(result.error);
              }
              keyId = result.keys.at(-1).id;
            }
            if (method === "publickey" && !keyId)
              throw new Error("Choose or import an SSH key.");
            finish({
              method,
              keyId,
              password:
                method === "password"
                  ? form.elements.password.value
                  : undefined,
              remember: method === "password" && form.elements.remember.checked,
            });
          } catch (e) {
            if (!signal.aborted)
              d.querySelector(".prompt-error").textContent = e.message;
            button.disabled = false;
          }
        };
      }),
  );
  queue = pending.catch(() => {});
  return pending;
}
