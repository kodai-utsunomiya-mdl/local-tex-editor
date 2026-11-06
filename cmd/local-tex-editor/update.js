const token = new URLSearchParams(location.hash.slice(1)).get('token') || '';
const byID = id => document.getElementById(id);
let state = null;
let working = false;
let closed = false;
let hidden = false;
let navigating = false;
let stream = null;
let retry = null;

function showError(message) {
  byID('update-error').textContent = message;
  byID('update-error').hidden = !message;
}

function render(value) {
  state = value;
  byID('update-current-version').textContent = `v${value.currentVersion}`;
  byID('update-requested-version').textContent = `v${value.requestedVersion}`;
  byID('update-executable').textContent = value.executable;
  byID('update-open-current').href = value.currentURL;
  const phase = value.phase;
  byID('update-status').textContent = phase === 'ready' ? '更新後の編集画面を開いています…'
    : phase === 'starting' ? '更新後のアプリを起動しています…'
      : phase === 'cancelled' ? '更新を取り消しました．このタブを閉じてください．'
        : value.stopping ? '起動中のアプリの終了を待っています…'
          : `v${value.currentVersion} の終了を待っています．`;
  byID('update-options').hidden = phase === 'ready' || phase === 'starting' || phase === 'cancelled';
  byID('update-stop-current').disabled = working || value.stopping || phase !== 'waiting' || !byID('update-saved').checked;
  byID('update-saved').disabled = working || value.stopping || phase !== 'waiting';
  showError(value.error || '');
  if (phase === 'cancelled') { closed = true; stream?.abort(); }
  if (phase === 'ready' && value.readyURL && !navigating) {
    const next = new URL(value.readyURL);
    if (next.protocol !== 'http:' || !(next.hostname.startsWith('127.') || next.hostname === '[::1]')) {
      showError('更新後のアプリへ接続できません．実行ファイルを開き直してください．');
      return;
    }
    navigating = true;
    location.replace(value.readyURL);
  }
}

async function request(path, body) {
  const response = await fetch(path, {
    method: body ? 'POST' : 'GET', cache: 'no-store',
    headers: { 'X-Local-Token': token, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
  let data;
  try { data = await response.json(); } catch { data = {}; }
  if (!response.ok) throw new Error(data.error || 'アプリに接続できません．実行ファイルを開き直してください．');
  return data;
}

async function connect() {
  if (closed || hidden || navigating || stream) return;
  const controller = new AbortController();
  stream = controller;
  try {
    const response = await fetch('/api/update/events', {
      headers: { 'X-Local-Token': token }, cache: 'no-store', signal: controller.signal,
    });
    if (!response.ok || !response.body) throw new Error('更新画面に接続できません．実行ファイルを開き直してください．');
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = '';
    while (!closed && !hidden && !navigating) {
      const result = await reader.read();
      if (result.done) break;
      pending += decoder.decode(result.value, { stream: true });
      let boundary;
      while ((boundary = pending.indexOf('\n\n')) >= 0) {
        const message = pending.slice(0, boundary);
        pending = pending.slice(boundary + 2);
        const data = message.split('\n').filter(line => line.startsWith('data: ')).map(line => line.slice(6)).join('\n');
        if (data) render(JSON.parse(data));
      }
    }
    await reader.cancel().catch(() => {});
  } catch (error) {
    if (!closed && !hidden && !navigating && error.name !== 'AbortError') showError(error.message);
  } finally {
    if (stream === controller) stream = null;
    if (!closed && !hidden && !navigating) retry = setTimeout(connect, 700);
  }
}

byID('update-saved').addEventListener('change', () => { if (state) render(state); });
byID('update-stop-current').addEventListener('click', async () => {
  if (working || !byID('update-saved').checked) return;
  working = true; if (state) render(state);
  let failure = '';
  try { await request('/api/update/stop', { saved: true }); }
  catch (error) { failure = error.message; }
  finally {
    working = false;
    if (state && !closed && !navigating) {
      render(state);
      if (failure) showError(failure);
    }
  }
});
byID('update-cancel').addEventListener('click', async () => {
  if (closed || navigating) return;
  try {
    await request('/api/update/cancel', {});
    closed = true; stream?.abort(); clearTimeout(retry);
    byID('update-options').hidden = true;
    byID('update-status').textContent = '更新を取り消しました．このタブを閉じてください．';
    showError('');
    try { window.close(); } catch { /* The message remains available in tabs the browser keeps open. */ }
  } catch (error) { showError(error.message); }
});
window.addEventListener('pagehide', () => { hidden = true; clearTimeout(retry); stream?.abort(); });
window.addEventListener('pageshow', event => { if (event.persisted) { hidden = false; connect(); } });

if (!token) showError('起動時に開いた URL を使用してください．');
else connect();
