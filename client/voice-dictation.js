import { createSpeechEngine } from "./speech-engine.js";
import { recordSpeech } from "./speech-audio.js";
import { sanitizePaste } from "./terminal-input.js";
import { endpointKey } from "./workspace-state.js";
export function dictationText(text) {
  // A dictation is always one insertion; line breaks must not submit shell input.
  return sanitizePaste(text)
    .replace(/[\r\n\t]+/g, " ")
    .trim();
}
export function setupVoiceDictation({ getActive, available, notice }) {
  let modal, cancelCurrent, modelPromise;
  const engine = createSpeechEngine(() => {});
  const load = () =>
    (modelPromise ||= engine.load().catch((error) => {
      modelPromise = null;
      throw error;
    }));
  // Warm the model after unlock. This never requests microphone access.
  void load().catch(() => {});
  function open() {
    if (modal?.open) {
      modal.querySelector("button:not([disabled])")?.focus();
      return;
    }
    if (!available() || document.querySelector("dialog[open]")) return;
    const target = getActive();
    if (!target || target.disposed || target.status !== "Connected") {
      notice("Connect a terminal before dictating.");
      return;
    }
    const endpoint = endpointKey(target.server);
    let alive = true,
      ready = false,
      busy = false,
      recorder,
      clock,
      generation = 0,
      started;
    modal = document.createElement("dialog");
    modal.id = "voice-dialog";
    modal.setAttribute("aria-labelledby", "voice-title");
    modal.innerHTML =
      '<div class="dialog-head"><h2 id="voice-title">Dictate</h2><button id="voice-close" aria-label="Cancel dictation">×</button></div><div class="voice-visual" data-state="idle"><div class="voice-wave" aria-hidden="true">' +
      "<span></span>".repeat(16) +
      '</div><div class="voice-listening">Starting microphone</div><div class="voice-time" role="progressbar" aria-label="Recording time" aria-valuemin="0" aria-valuemax="60" aria-valuenow="0"><span></span></div></div><p id="voice-status" role="status">Starting microphone...</p><div class="dialog-actions"><button id="voice-record" class="primary" hidden>Record again</button><button id="voice-stop" class="primary" disabled>Stop</button></div>';
    const view = modal,
      $ = (s) => view.querySelector(s),
      status = $("#voice-status"),
      record = $("#voice-record"),
      stop = $("#voice-stop"),
      visual = $(".voice-visual"),
      listening = $(".voice-listening"),
      bars = [...view.querySelectorAll(".voice-wave span")],
      time = $(".voice-time"),
      elapsed = time.querySelector("span");
    const quiet = (label) => {
      visual.dataset.state = "idle";
      listening.textContent = label;
      bars.forEach((bar) => bar.style.setProperty("--level", "0"));
    };
    const updateStatus = () => {
      if (!recorder) return;
      const seconds = Math.min(60, (Date.now() - started) / 1000);
      elapsed.style.width = `${(seconds / 60) * 100}%`;
      const rounded = String(Math.floor(seconds));
      if (time.getAttribute("aria-valuenow") !== rounded) {
        time.setAttribute("aria-valuenow", rounded);
        time.setAttribute(
          "aria-valuetext",
          `${rounded} seconds recorded; 60 second limit`,
        );
      }
      const text = ready ? "" : "Loading speech model...";
      if (status.textContent !== text) status.textContent = text;
    };
    const setBusy = (value) => {
      busy = value;
      record.hidden = value;
      stop.hidden = !value;
    };
    const cleanup = () => {
      if (!alive) return;
      alive = false;
      generation++;
      modelPromise = null;
      clearInterval(clock);
      recorder?.cancel();
      recorder = null;
      engine.cancel();
      $("#voice-transcript")?.remove();
      view.close();
      view.remove();
      if (modal === view) modal = null;
    };
    cancelCurrent = cleanup;
    $("#voice-close").onclick = cleanup;
    view.oncancel = (e) => {
      e.preventDefault();
      cleanup();
    };
    view.onclose = cleanup;
    function fail(error) {
      if (!alive) return;
      generation++;
      clearInterval(clock);
      recorder?.cancel();
      recorder = null;
      stop.disabled = true;
      engine.cancel();
      modelPromise = null;
      ready = false;
      quiet("Microphone off");
      setBusy(false);
      status.textContent =
        error.name === "NotAllowedError"
          ? "Microphone access was denied. Allow it in your browser’s site settings and try again."
          : error.message;
    }
    const audible = (samples) => {
      let energy = 0;
      for (const sample of samples) energy += sample * sample;
      return samples.length && Math.sqrt(energy / samples.length) >= 0.001;
    };
    async function finish() {
      if (!recorder) return;
      const run = generation;
      const captured = recorder;
      recorder = null;
      stop.disabled = true;
      clearInterval(clock);
      quiet("Transcribing...");
      status.textContent = "";
      try {
        const samples = await captured.stop();
        if (!alive || generation !== run) return;
        await load();
        if (!alive || generation !== run) return;
        if (!audible(samples))
          throw Error(
            "No speech detected. Check the microphone and try again.",
          );
        const text = dictationText(await engine.transcribe(samples));
        if (!alive || generation !== run) return;
        if (!text) throw Error("No speech recognized. Try recording again.");
        try {
          if (
            target.disposed ||
            target.status !== "Connected" ||
            !target.send ||
            getActive()?.id !== target.id ||
            endpointKey(target.server) !== endpoint
          )
            throw Error(
              "The terminal changed or disconnected. Your text is below so you can copy it.",
            );
          target.history?.close();
          target.term.paste(text);
        } catch (error) {
          const recovery = document.createElement("textarea");
          recovery.id = "voice-transcript";
          recovery.setAttribute("aria-label", "Unsent dictation");
          recovery.value = text;
          view.append(recovery);
          throw error;
        }
        cleanup();
        target.term.focus();
      } catch (error) {
        if (alive && generation === run) fail(error);
      }
    }
    async function start() {
      if (busy) return;
      const run = ++generation;
      setBusy(true);
      elapsed.style.width = "0%";
      time.setAttribute("aria-valuenow", "0");
      $("#voice-transcript")?.remove();
      status.textContent = "Waiting for microphone permission...";
      void load()
        .then(() => {
          if (alive && generation === run) {
            ready = true;
            updateStatus();
          }
        })
        .catch((error) => {
          if (alive && generation === run) fail(error);
        });
      try {
        const captured = await recordSpeech({
          cancelled: () => !alive || generation !== run,
          onLimit: () => void finish(),
          onLevel: (levels) => {
            if (!alive || generation !== run || !recorder) return;
            visual.dataset.state = "listening";
            bars.forEach((bar, i) =>
              bar.style.setProperty("--level", levels[i].toFixed(3)),
            );
            listening.textContent =
              Math.max(...levels) > 0.12 ? "Sound detected" : "Listening...";
          },
        });
        if (!alive || generation !== run) {
          captured.cancel();
          return;
        }
        recorder = captured;
        visual.dataset.state = "listening";
        listening.textContent = "Listening...";
        started = Date.now();
        updateStatus();
        clock = setInterval(updateStatus, 250);
        stop.disabled = false;
        stop.focus();
      } catch (error) {
        if (alive && generation === run) fail(error);
      }
    }
    record.onclick = start;
    stop.onclick = finish;
    (document.fullscreenElement || document.body).append(view);
    view.showModal();
    void start();
  }
  window.addEventListener(
    "keydown",
    (e) => {
      if (
        e.altKey &&
        ((e.shiftKey && (e.code === "KeyV" || e.key.toLowerCase() === "v")) ||
          (!e.shiftKey && (e.code === "Space" || e.key === " "))) &&
        !e.metaKey &&
        !e.ctrlKey &&
        !e.isComposing &&
        available()
      ) {
        e.preventDefault();
        e.stopImmediatePropagation();
        open();
      }
    },
    true,
  );
  return {
    open,
    cancel: () => {
      cancelCurrent?.();
      engine.cancel();
      modelPromise = null;
    },
  };
}
