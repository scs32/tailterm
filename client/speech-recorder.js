class DictationRecorder extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(2048);
    this.used = 0;
    this.port.onmessage = ({ data }) => {
      if (data === "flush") {
        this.flush();
        this.port.postMessage({ flushed: true });
      }
    };
  }
  flush() {
    if (this.used) {
      const samples = this.buffer.slice(0, this.used);
      this.port.postMessage({ samples }, [samples.buffer]);
      this.used = 0;
    }
  }
  process(inputs) {
    const channels = inputs[0];
    if (!channels?.length) return true;
    for (let i = 0; i < channels[0].length; i++) {
      let sum = 0;
      for (const channel of channels) sum += channel[i];
      this.buffer[this.used++] = sum / channels.length;
      if (this.used === this.buffer.length) this.flush();
    }
    return true;
  }
}
registerProcessor("tailterm-dictation-recorder", DictationRecorder);
