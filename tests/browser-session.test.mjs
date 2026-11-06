import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';
import test from 'node:test';
import { BrowserSession } from '../web/src/browser-session.js';

function streamingServer() {
  const connections = [];
  const request = async (url, options) => {
    const connection = { url, options };
    connections.push(connection);
    const body = new ReadableStream({
      start(controller) {
        connection.close = () => controller.close();
        options.signal.addEventListener('abort', () => controller.error(new DOMException('Closed', 'AbortError')), { once: true });
      },
    });
    return { ok: true, body };
  };
  return { connections, request };
}

test('the default fetch receives the browser global object as its receiver', async context => {
  const server = streamingServer();
  const statuses = [];
  const receivers = [];
  context.mock.method(globalThis, 'fetch', function (...args) {
    receivers.push(this);
    if (this !== globalThis) throw new TypeError('Illegal invocation');
    return server.request(...args);
  });
  const session = new BrowserSession({ token: 'local-token', onConnectionChange: value => statuses.push(value) });
  try {
    session.start();
    await delay(0);
    assert.equal(receivers.length, 1);
    assert.equal(receivers[0], globalThis);
    assert.equal(server.connections.length, 1);
    assert.deepEqual(statuses, [true]);
  } finally {
    session.stop();
  }
});

test('one tab opens one authenticated stream and aborts it when stopped', async () => {
  const server = streamingServer();
  const statuses = [];
  const session = new BrowserSession({ token: 'local-token', fetch: server.request, onConnectionChange: value => statuses.push(value), retryDelays: [1] });
  session.start(); session.start();
  await delay(0);
  assert.equal(server.connections.length, 1);
  assert.equal(server.connections[0].url, '/api/session');
  assert.equal(server.connections[0].options.headers['X-Local-Token'], 'local-token');
  assert.deepEqual(statuses, [true]);
  session.stop();
  assert.equal(server.connections[0].options.signal.aborted, true);
  await delay(10);
  assert.equal(server.connections.length, 1);
});

test('a resumed tab reconnects while the old stream finishes aborting', async () => {
  const server = streamingServer();
  const session = new BrowserSession({ token: 'local-token', fetch: server.request, retryDelays: [1] });
  session.start();
  await delay(0);
  session.stop(); session.start();
  await delay(0);
  assert.equal(server.connections.length, 2);
  assert.equal(server.connections[1].options.signal.aborted, false);
  session.start();
  assert.equal(server.connections.length, 2);
  session.stop();
});

test('a disconnected stream reconnects and stopping cancels a pending retry', async () => {
  const server = streamingServer();
  const statuses = [];
  const session = new BrowserSession({ token: 'local-token', fetch: server.request, onConnectionChange: value => statuses.push(value), retryDelays: [1] });
  session.start();
  await delay(0);
  server.connections[0].close();
  await delay(10);
  assert.equal(server.connections.length, 2);
  assert.deepEqual(statuses, [true, false, true]);
  server.connections[1].close();
  await delay(0);
  session.stop();
  const count = server.connections.length;
  await delay(10);
  assert.equal(server.connections.length, count);
});

test('a brief fetch failure retries without requiring the editor to reload', async () => {
  const server = streamingServer();
  let attempts = 0;
  const session = new BrowserSession({
    token: 'local-token', retryDelays: [1],
    fetch: (...args) => ++attempts === 1 ? Promise.reject(new TypeError('Connection interrupted')) : server.request(...args),
  });
  session.start();
  await delay(10);
  assert.equal(attempts, 2);
  assert.equal(server.connections.length, 1);
  session.stop();
});
