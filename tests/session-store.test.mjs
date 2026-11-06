import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import test from 'node:test';
import { createStoragePatch, SessionStore } from '../web/src/session-store.js';

function snapshot(value) {
  const serialized = value === undefined ? '' : JSON.stringify(value);
  return { value: value ?? null, serialized, revision: serialized ? createHash('sha256').update(serialized).digest('hex') : '' };
}

function applyPatch(serialized, patch) {
  const bytes = Buffer.from(serialized);
  const chunks = [];
  let offset = 0;
  for (const edit of patch.edits) {
    assert.ok(edit.prefixBytes >= offset);
    const end = bytes.length - edit.suffixBytes;
    assert.ok(end >= edit.prefixBytes);
    chunks.push(bytes.subarray(offset, edit.prefixBytes), Buffer.from(edit.insert));
    offset = end;
  }
  chunks.push(bytes.subarray(offset));
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

async function until(predicate) {
  const deadline = Date.now() + 2000;
  while (!predicate()) {
    if (Date.now() > deadline) assert.fail('The asynchronous storage request did not finish.');
    await delay(1);
  }
}

function storageServer(initial = {}) {
  const values = new Map(Object.entries(initial).map(([key, value]) => [key, snapshot(value)]));
  const calls = [];
  const current = key => values.get(key) || snapshot(undefined);
  const api = async (url, options = {}) => {
    const method = options.method || 'GET';
    const key = method === 'GET' ? new URL(url, 'http://localhost').searchParams.get('key') : options.body.key;
    calls.push({ url, method, ...options });
    if (method === 'GET') return current(key);
    const base = current(key);
    const conflict = () => { throw Object.assign(new Error('Storage changed'), { status: 409, data: base }); };
    let value;
    if (method === 'PUT') {
      if (options.body.baseRevision !== undefined && options.body.baseRevision !== base.revision) conflict();
      value = options.body.value;
    } else if (method === 'PATCH') {
      const patch = options.body.patches.find(item => item.baseRevision === base.revision);
      if (!patch) conflict();
      value = applyPatch(base.serialized, patch);
    } else assert.fail(`Unexpected storage method: ${method}`);
    const result = snapshot(value);
    values.set(key, result);
    return result;
  };
  return { api, calls, current, values };
}

function draft(content, updatedAt = 100) {
  return {
    version: 1, updatedAt,
    entries: [{ id: 'main-draft', path: 'main.tex', baseRevision: 'disk-revision', content, updatedAt,
      selection: { anchor: Math.floor(content.length / 2), head: Math.floor(content.length / 2) } }],
  };
}

test('storage patches use complete UTF-8 characters for Japanese and surrogate pairs', () => {
  for (const [old, next] of [
    ['日本語の原稿', '日本語の新しい原稿'],
    ['😀 and 😀', '😁 and 😀'],
    ['\u{10000}末尾', '\u{10400}末尾'],
    ['a😀b', 'aX😀b'],
    ['a😀b', 'a😀Xb'],
    ['日本語\n\\command{<>&}\u2028\u2029', '日本語\n\\command{<変更>&}\u2028\u2029'],
    ['\ud800', '\ud801'],
  ]) {
    const before = snapshot({ content: old });
    const value = { content: next };
    assert.deepEqual(applyPatch(before.serialized, createStoragePatch(before, JSON.stringify(value))), value);
  }
});

test('a large draft can send a middle edit, timestamps and caret movement within the keepalive limit', async () => {
  const key = 'drafts:日本語の原稿';
  const content = '% 長い原稿 <>& 😀 の本文\n'.repeat(10000);
  const initial = draft(content);
  assert.ok(Buffer.byteLength(JSON.stringify(initial)) > 300_000);
  const server = storageServer({ [key]: initial });
  const store = new SessionStore(server.api);
  await store.read(key);
  const middle = content.indexOf('\n', Math.floor(content.length / 2));
  const edited = draft(`${content.slice(0, middle)}追記${content.slice(middle)}`, 200);
  edited.entries[0].selection = { anchor: middle + 2, head: middle + 2 };
  store.write(key, edited, 60_000);
  store.flushOnHide();
  await until(() => !store.pending);
  const sent = server.calls.find(call => call.method === 'PATCH');
  assert.equal(sent.keepalive, true);
  assert.equal(sent.timeout, 0);
  assert.ok(sent.body.patches[0].edits.length >= 4);
  assert.ok(Buffer.byteLength(JSON.stringify(sent.body)) < 2000);
  assert.deepEqual(server.current(key).value, edited);
});

test('a new small draft and a change to the JSON shape can be restored', async () => {
  const key = 'drafts:new';
  const server = storageServer();
  const store = new SessionStore(server.api);
  await store.read(key);
  const initial = draft('新しい草稿\n');
  store.write(key, initial, 60_000);
  store.flushOnHide();
  await until(() => !store.pending);
  assert.deepEqual(server.current(key).value, initial);
  await store.resume();
  const updated = { ...initial, entries: [...initial.entries, { id: 'notes', content: '別の草稿' }] };
  store.write(key, updated, 60_000);
  store.flushOnHide();
  await until(() => !store.pending);
  assert.deepEqual(server.current(key).value, updated);
});

for (const committed of [false, true]) {
  test(`a close-time patch survives an in-flight PUT that ${committed ? 'has committed without a response' : 'has not committed yet'}`, async () => {
    const key = 'drafts:concurrent';
    const initial = draft('前の内容\n'.repeat(20000));
    const server = storageServer({ [key]: initial });
    const gate = deferred();
    let entered = false;
    const api = async (url, options = {}) => {
      if (options.method !== 'PUT') return server.api(url, options);
      const response = committed ? await server.api(url, options) : undefined;
      entered = true;
      await gate.promise;
      return committed ? response : server.api(url, options);
    };
    const store = new SessionStore(api);
    await store.read(key);
    store.write(key, draft(`${initial.entries[0].content}一つ前の入力`, 200), 60_000);
    const writing = store.flush();
    await until(() => entered);
    const final = draft(`${initial.entries[0].content}一つ前の入力と最後の入力`, 300);
    store.write(key, final, 60_000);
    store.flushOnHide();
    await until(() => !store.pending);
    const patch = server.calls.find(call => call.method === 'PATCH');
    assert.equal(patch.body.patches.length, 2);
    gate.resolve();
    await writing;
    assert.deepEqual(server.current(key).value, final);
    assert.equal(store.pending, false);
  });
}

test('pageshow can save a newer edit while its own earlier close-time patch is awaiting a response', async () => {
  const key = 'drafts:resumed';
  const server = storageServer({ [key]: draft('original') });
  const putGate = deferred(), patchGate = deferred();
  let putStarted = false, patchStarted = false;
  const api = async (url, options = {}) => {
    if (options.method === 'PUT' && !putStarted) {
      putStarted = true;
      await putGate.promise;
    }
    const response = await server.api(url, options);
    if (options.method === 'PATCH') { patchStarted = true; await patchGate.promise; }
    return response;
  };
  const store = new SessionStore(api);
  await store.read(key);
  store.write(key, draft('first edit', 200), 60_000);
  const writing = store.flush();
  await until(() => putStarted);
  store.write(key, draft('at pagehide', 300), 60_000);
  store.flushOnHide();
  await until(() => patchStarted);
  const latest = draft('typed after pageshow', 400);
  store.write(key, latest, 60_000);
  const resumed = store.resume();
  putGate.resolve();
  await Promise.all([writing, resumed]);
  patchGate.resolve();
  await delay(0);
  assert.deepEqual(server.current(key).value, latest);
  assert.equal(store.pending, false);
});

test('a draft acknowledged during hashing cannot be overwritten when the tab resumes', async context => {
  const key = 'drafts:hashing';
  const server = storageServer({ [key]: draft('original') });
  const store = new SessionStore(server.api);
  const gate = deferred();
  const digest = crypto.subtle.digest.bind(crypto.subtle);
  let hashing = false;
  context.mock.method(crypto.subtle, 'digest', async (...args) => {
    hashing = true;
    await gate.promise;
    return digest(...args);
  });
  await store.read(key);
  store.write(key, draft('earlier input', 200), 60_000);
  const writing = store.flush();
  await until(() => hashing);
  const latest = draft('input recorded while hashing was pending', 300);
  store.write(key, latest, 60_000);
  store.flushOnHide();
  await until(() => !store.pending);
  const resumed = store.resume();
  gate.resolve();
  await Promise.all([writing, resumed]);
  assert.deepEqual(server.current(key).value, latest);
  assert.equal(server.calls.filter(call => call.method === 'PUT').length, 0);
  assert.equal(store.pending, false);
});

test('a write from another tab is left in place until the user requests another save', async () => {
  const key = 'drafts:conflict';
  const server = storageServer({ [key]: draft('previous') });
  const store = new SessionStore(server.api);
  await store.read(key);
  const anotherTab = draft('saved by another tab', 200);
  server.values.set(key, snapshot(anotherTab));
  const latest = draft('latest local input', 300);
  store.write(key, latest, 60_000);
  await assert.rejects(store.flush(), { status: 409 });
  assert.deepEqual(server.current(key).value, anotherTab);
  assert.equal(server.calls.filter(call => call.method === 'PUT').length, 1);
  store.write(key, draft('continued editing', 400), 60_000);
  await assert.rejects(store.flush(), { status: 409 });
  assert.deepEqual(server.current(key).value, anotherTab);
  store.write(key, latest, 60_000);
  await store.flush({ retryConflicts: true });
  assert.deepEqual(server.current(key).value, latest);
  assert.equal(server.calls.filter(call => call.method === 'PUT').length, 2);
});

test('tabs can update editing positions and preferences without a draft conflict', async () => {
  for (const key of ['session:project', 'preferences']) {
    const server = storageServer({ [key]: { position: 0 } });
    const first = new SessionStore(server.api), second = new SessionStore(server.api);
    await first.read(key); await second.read(key);
    first.write(key, { position: 1 }, 60_000);
    await first.flush();
    second.write(key, { position: 2 }, 60_000);
    await second.flush();
    assert.deepEqual(server.current(key).value, { position: 2 });
    first.write(key, { position: 3 }, 60_000);
    first.flushOnHide();
    await until(() => !first.pending);
    assert.deepEqual(server.current(key).value, { position: 3 });
    const writes = server.calls.filter(call => call.method === 'PUT');
    assert.equal(writes.length, 3);
    assert.ok(writes.every(call => call.body.baseRevision === undefined));
    assert.equal(writes[2].keepalive, true);
  }
});

test('all keepalive requests share one budget and deferred work resumes on pageshow', async () => {
  const server = storageServer();
  const store = new SessionStore(server.api);
  const keys = ['session:first', 'drafts:important', 'drafts:another'];
  for (const key of keys) {
    await store.read(key);
    store.write(key, { content: 'x'.repeat(28_000) }, 60_000);
  }
  store.flushOnHide();
  await delay(0);
  const sent = server.calls.filter(call => call.method === 'PATCH');
  assert.equal(sent.length, 2);
  assert.ok(sent.every(call => call.body.key.startsWith('drafts:')));
  assert.ok(sent.reduce((sum, call) => sum + Buffer.byteLength(JSON.stringify(call.body)), 0) <= 60 * 1024);
  assert.equal(store.pending, true);
  await store.resume();
  assert.equal(store.pending, false);
  assert.deepEqual(server.current('session:first').value, { content: 'x'.repeat(28_000) });
});

test('a fresh large paste stays pending until its regular save completes', async () => {
  const key = 'drafts:large-paste';
  const server = storageServer();
  const store = new SessionStore(server.api);
  await store.read(key);
  const value = draft('新しく貼り付けた本文\n'.repeat(30000));
  store.write(key, value, 60_000);
  store.flushOnHide();
  assert.equal(store.pending, true);
  assert.equal(server.calls.filter(call => call.method === 'PATCH').length, 0);
  await store.resume();
  assert.equal(store.pending, false);
  assert.deepEqual(server.current(key).value, value);
});
