// Keep one connection open for each editor tab. The local application waits
// briefly after the final connection closes, so a reload can reconnect.
export class BrowserSession {
  constructor({ token, fetch: request = globalThis.fetch, onConnectionChange, retryDelays = [250, 500, 1000] }) {
    this.token = token;
    this.request = request;
    this.onConnectionChange = onConnectionChange;
    this.retryDelays = retryDelays;
    this.retry = 0;
    this.controller = null;
    this.timer = null;
    this.active = false;
  }

  start() {
    if (!this.token) return;
    this.active = true;
    if (!this.controller && !this.timer) this.connect();
  }

  stop() {
    this.active = false;
    clearTimeout(this.timer); this.timer = null;
    const controller = this.controller;
    this.controller = null;
    controller?.abort();
    this.retry = 0;
  }

  async connect() {
    if (!this.active || this.controller) return;
    const controller = new AbortController();
    this.controller = controller;
    let reader;
    try {
      const response = await this.request('/api/session', {
        headers: { 'X-Local-Token': this.token, Accept: 'text/event-stream' },
        cache: 'no-store', signal: controller.signal,
      });
      if (!response.ok || !response.body) throw new Error('The browser session could not connect.');
      if (this.controller !== controller || !this.active) { await response.body.cancel(); return; }
      this.retry = 0;
      this.onConnectionChange?.(true);
      reader = response.body.getReader();
      while (!(await reader.read()).done) { /* Reading the stream also observes a disconnected server. */ }
    } catch { /* A short retry covers a temporary interruption on the loopback connection. */ }
    finally {
      reader?.releaseLock();
      if (this.controller !== controller) return;
      this.controller = null;
      if (!this.active || controller.signal.aborted) return;
      this.onConnectionChange?.(false);
      const delay = this.retryDelays[Math.min(this.retry++, this.retryDelays.length - 1)] ?? 1000;
      this.timer = setTimeout(() => { this.timer = null; this.connect(); }, delay);
    }
  }
}
