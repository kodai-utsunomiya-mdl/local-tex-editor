// Coalesce frequent editor changes and write each key in order.
export class SessionStore {
  constructor(api, onStatus) {
    this.api = api;
    this.onStatus = onStatus;
    this.records = new Map();
  }

  async read(key) {
    const { value } = await this.api(`/api/storage?key=${encodeURIComponent(key)}`, { projectRoot: null });
    let backup = null;
    try { backup = JSON.parse(localStorage.getItem(`local-tex-editor-backup:${key}`) || 'null'); } catch { /* The PC copy remains available when browser storage is disabled. */ }
    if (backup?.value && backup.updatedAt > (Number(value?.updatedAt) || 0)) {
      this.write(key, backup.value, 0);
      return backup.value;
    }
    return value;
  }

  write(key, value, delay = 500) {
    let record = this.records.get(key);
    if (!record) {
      record = { value: null, version: 0, saved: 0, timer: null, running: null };
      this.records.set(key, record);
    }
    record.value = value;
    ++record.version;
    clearTimeout(record.timer);
    if (key.startsWith('drafts:')) {
      try { localStorage.setItem(`local-tex-editor-backup:${key}`, JSON.stringify({ value, updatedAt: value.updatedAt || Date.now() })); } catch { /* The server write below is independent of browser quota. */ }
    }
    this.onStatus?.('saving', key);
    record.timer = setTimeout(() => this.flushKey(key).catch(() => {}), delay);
  }

  async flushKey(key) {
    const record = this.records.get(key);
    if (!record) return;
    clearTimeout(record.timer); record.timer = null;
    if (record.running) return record.running;
    if (record.saved === record.version) return;
    const work = (async () => {
      while (record.saved < record.version) {
        const version = record.version;
        const value = record.value;
        await this.api('/api/storage', { method: 'PUT', body: { key, value }, projectRoot: null, timeout: 20000 });
        record.saved = version;
        if (record.saved === record.version) {
          try { localStorage.removeItem(`local-tex-editor-backup:${key}`); } catch { /* Optional cache. */ }
        }
      }
      this.onStatus?.('saved', key);
    })();
    record.running = work;
    try { await work; }
    catch (error) { this.onStatus?.('error', key, error); throw error; }
    finally { record.running = null; }
  }

  async flush() {
    const failures = await Promise.allSettled([...this.records.keys()].map(key => this.flushKey(key)));
    const error = failures.find(result => result.status === 'rejected');
    if (error) throw error.reason;
  }

  get pending() { return [...this.records.values()].some(record => record.saved < record.version); }
}
