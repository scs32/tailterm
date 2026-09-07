// Audio is unlocked by a user gesture; background events never queue stale sounds.
export function createAttentionSound({
  enabled,
  createContext = () =>
    new (window.AudioContext || window.webkitAudioContext)(),
  now = () => performance.now(),
}) {
  let context;
  let lastPlayed = -Infinity;
  async function unlock() {
    try {
      context ||= createContext();
      if (context.state !== "running") await context.resume();
      return context.state === "running";
    } catch {
      return false;
    }
  }
  function play(preview = false) {
    if (!preview && (!enabled() || now() - lastPlayed < 5000)) return false;
    if (context?.state !== "running") return false;
    try {
      const start = context.currentTime;
      for (const [frequency, delay] of [
        [523.25, 0],
        [659.25, 0.12],
      ]) {
        const oscillator = context.createOscillator();
        const gain = context.createGain();
        oscillator.type = "sine";
        oscillator.frequency.value = frequency;
        gain.gain.setValueAtTime(0, start + delay);
        gain.gain.linearRampToValueAtTime(0.075, start + delay + 0.015);
        gain.gain.exponentialRampToValueAtTime(0.001, start + delay + 0.4);
        oscillator.connect(gain);
        gain.connect(context.destination);
        oscillator.onended = () => {
          oscillator.disconnect();
          gain.disconnect();
        };
        oscillator.start(start + delay);
        oscillator.stop(start + delay + 0.42);
      }
      lastPlayed = now();
      return true;
    } catch {
      return false;
    }
  }
  return {
    unlock,
    notify(label) {
      if (["Bell", "Command finished", "Needs attention"].includes(label))
        play();
    },
    async preview() {
      return (await unlock()) && play(true);
    },
  };
}
