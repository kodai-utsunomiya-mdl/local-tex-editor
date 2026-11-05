import { test as base, expect } from '@playwright/test';
import { execFile, spawn } from 'node:child_process';
import { once } from 'node:events';
import { cp, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { promisify } from 'node:util';

const repository = path.resolve(import.meta.dirname, '..');
const fixture = path.join(import.meta.dirname, 'fixtures', 'japanese');
const executable = path.resolve(process.env.LOCAL_TEX_EDITOR_TEST_EXE ||
  path.join(repository, 'bin', process.platform === 'win32' ? 'local-tex-editor.exe' : 'local-tex-editor'));
const execFileAsync = promisify(execFile);

async function waitUntil(read, predicate, timeout, description) {
  const deadline = Date.now() + timeout;
  let latest;
  while (Date.now() < deadline) {
    latest = await read();
    if (predicate(latest)) return latest;
    await delay(150);
  }
  throw new Error(`${description}\nLast result: ${JSON.stringify(latest)}`);
}

async function request(session, endpoint, { method = 'GET', data, token = true, headers = {} } = {}) {
  const response = await fetch(new URL(endpoint, session.url), {
    method,
    headers: {
      ...(token ? { 'X-Local-Token': session.token } : {}),
      Origin: new URL(session.url).origin,
      ...(data === undefined ? {} : { 'Content-Type': 'application/json' }),
      ...headers,
    },
    body: data === undefined ? undefined : JSON.stringify(data),
    signal: AbortSignal.timeout(20_000),
  });
  return response;
}

async function json(session, endpoint, options) {
  const response = await request(session, endpoint, options);
  const content = await response.text();
  if (!response.ok) throw new Error(`${options?.method || 'GET'} ${endpoint}: HTTP ${response.status}\n${content}`);
  return JSON.parse(content);
}

async function openProject(session) {
  return json(session, '/api/project/open', { method: 'POST', data: { path: session.project } });
}

async function buildFinished(session, id) {
  return waitUntil(() => json(session, '/api/build'),
    build => build?.id === id && build.status !== 'running',
    100_000, 'The TeX process did not finish.');
}

async function compileFromUI(page, session) {
  const responsePromise = page.waitForResponse(response =>
    new URL(response.url()).pathname === '/api/build' && response.request().method() === 'POST');
  await page.getByTestId('compile-button').click();
  const response = await responsePromise;
  expect(response.ok(), await response.text()).toBe(true);
  const started = await response.json();
  return { started, finished: await buildFinished(session, started.id) };
}

async function replaceEditor(page, content) {
  await page.getByTestId('editor').locator('.cm-content').click();
  await page.keyboard.press('Control+A');
  await page.keyboard.insertText(content);
}

function redactOutput(text, token) {
  return token ? text.replaceAll(token, '[local session token]') : text;
}

const test = base.extend({
  session: async ({}, use, testInfo) => {
    await stat(executable).catch(() => {
      throw new Error(`Build the application before running browser tests: ${executable}`);
    });
    const temp = await mkdtemp(path.join(os.tmpdir(), 'ltex-'));
    const project = path.join(temp, '日本語の原稿 with spaces');
    await cp(fixture, project, { recursive: true });
    const readyFile = path.join(temp, 'ready.json');
    const args = ['--no-browser', '--ready-file', readyFile, '--config-dir', path.join(temp, 'config')];
    if (process.env.LOCAL_TEX_EDITOR_TEST_TEX_BIN) {
      args.push('--tex-bin', process.env.LOCAL_TEX_EDITOR_TEST_TEX_BIN);
    }
    let stdout = '';
    let stderr = '';
    let startupError;
    let session;
    const processHandle = spawn(executable, args, {
      cwd: repository,
      windowsHide: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    processHandle.on('error', error => { startupError = error; });
    processHandle.stdout.on('data', chunk => { stdout += chunk.toString('utf8'); });
    processHandle.stderr.on('data', chunk => { stderr += chunk.toString('utf8'); });
    try {
      const ready = await waitUntil(async () => {
        if (startupError) throw startupError;
        if (processHandle.exitCode !== null) {
          throw new Error(`Application exited with code ${processHandle.exitCode}.\n${stderr}\n${stdout}`);
        }
        try { return JSON.parse(await readFile(readyFile, 'utf8')); }
        catch (error) {
          if (error.code === 'ENOENT' || error instanceof SyntaxError) return null;
          throw error;
        }
      }, value => Boolean(value?.url && value?.token), 20_000, 'Application did not write its ready file.');
      session = { ...ready, project, temp };
      await use(session);
    } finally {
      if (session && processHandle.exitCode === null) {
        await request(session, '/api/shutdown', { method: 'POST', data: {} }).catch(() => {});
      }
      if (processHandle.exitCode === null && processHandle.signalCode === null) {
        await Promise.race([once(processHandle, 'exit').catch(() => {}), delay(4000)]);
      }
      if (processHandle.exitCode === null && processHandle.signalCode === null && processHandle.pid) {
        if (process.platform === 'win32') {
          const killer = spawn('taskkill.exe', ['/PID', String(processHandle.pid), '/T', '/F'], {
            windowsHide: true, stdio: 'ignore',
          });
          await Promise.race([once(killer, 'exit').catch(() => {}), delay(3000)]);
        } else {
          processHandle.kill('SIGKILL');
        }
      }
      if (testInfo.status !== testInfo.expectedStatus) {
        await testInfo.attach('application-output.txt', {
          body: Buffer.from(redactOutput(`${stdout}\n${stderr}`, session?.token)),
          contentType: 'text/plain',
        });
      }
      await rm(temp, { recursive: true, force: true, maxRetries: 6, retryDelay: 200 });
    }
  },
});

test('起動時のエラーを無人実行でも受け取れる', async () => {
  const result = await execFileAsync(executable, ['--no-browser', '--listen', '0.0.0.0:0'], {
    windowsHide: true,
    timeout: 10_000,
  }).then(output => ({ ...output, code: 0, killed: false }), error => error);
  expect(result.code).toBe(1);
  expect(Boolean(result.killed)).toBe(false);
  expect(result.stderr).toContain('loopback');
});

test('日本語の原稿を編集し，参考文献と PDF を更新する', async ({ page, session }, testInfo) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  if (!await page.getByTestId('project-path').isVisible()) {
    await page.getByRole('button', { name: 'プロジェクトフォルダーを開く', exact: false }).click();
  }
  await page.getByTestId('project-path').fill(session.project);
  await page.getByTestId('open-project').click();
  const editor = page.getByTestId('editor').locator('.cm-content');
  await expect(editor).toContainText('Local TeX Editor');
  await editor.click();
  await page.keyboard.press('Control+Home');
  const comment = '% ブラウザから保存した日本語のコメント\n';
  await page.keyboard.insertText(comment);

  const saveResponse = page.waitForResponse(response =>
    new URL(response.url()).pathname === '/api/file' && response.request().method() === 'PUT');
  await page.getByTestId('save-button').click();
  expect((await saveResponse).ok()).toBe(true);
  expect(await readFile(path.join(session.project, 'main.tex'), 'utf8')).toContain(comment.trim());

  const { started, finished } = await compileFromUI(page, session);
  expect(finished.status, finished.log + '\n' + finished.error).toBe('success');
  expect(finished.exitCode).toBe(0);
  expect(finished.log).toContain('LOCAL_TEX_EDITOR_RC_LOADED');
  expect(finished.log).toMatch(/uplatex/i);
  expect(finished.log).toMatch(/upbibtex/i);
  expect(finished.pdfReady).toBe(true);
  expect(finished.pdfBuildId).toBe(started.id);

  const bibliography = await readFile(path.join(session.project, '.local-tex-editor', 'build', 'main.bbl'), 'utf8');
  expect(bibliography).toContain('sample2026');
  const finalLog = await readFile(path.join(session.project, '.local-tex-editor', 'build', 'main.log'), 'utf8');
  expect(finalLog).not.toMatch(/There were undefined citations|Citation .* undefined/);

  const range = await request(session, '/api/pdf', { headers: { Range: 'bytes=0-4' } });
  expect(range.status).toBe(206);
  expect(range.headers.get('content-range')).toMatch(/^bytes 0-4\/\d+$/);
  expect(Buffer.from(await range.arrayBuffer()).toString('ascii')).toBe('%PDF-');
  await expect(page.getByTestId('build-status')).toHaveAttribute('data-status', 'success');
  const canvas = page.getByTestId('pdf-canvas');
  await expect(canvas).toBeVisible();
  await expect(canvas).toHaveAttribute('data-ready', 'true');
  await expect(canvas).toHaveAttribute('data-build-id', finished.pdfBuildId);
  await expect.poll(() => canvas.evaluate(element => {
    const context = element.getContext('2d');
    if (!context || element.width < 300 || element.height < 300) return false;
    const pixels = context.getImageData(0, 0, element.width, element.height).data;
    let dark = 0;
    for (let i = 0; i < pixels.length; i += 4 * 7) {
      if (pixels[i + 3] > 0 && pixels[i] < 220 && pixels[i + 1] < 220 && pixels[i + 2] < 220) dark++;
    }
    return dark > 20;
  })).toBe(true);
  await page.getByTestId('pdf-next').click();
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  await expect(canvas).toHaveAttribute('data-page', '2');

  await editor.click();
  await page.keyboard.press('Control+Home');
  await page.keyboard.insertText('% 再コンパイル後のページを確認する\n');
  const updated = await compileFromUI(page, session);
  expect(updated.finished.status, updated.finished.log).toBe('success');
  await expect(page.getByTestId('build-status')).toHaveAttribute('data-status', 'success');
  await expect(canvas).toHaveAttribute('data-build-id', updated.finished.pdfBuildId);
  await expect(canvas).toHaveAttribute('data-ready', 'true');
  await expect(canvas).toHaveAttribute('data-page', '2');
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  await page.screenshot({ path: testInfo.outputPath('editor-with-pdf.png') });
  expect(errors).toEqual([]);
});

test('保存の競合と，プロジェクト外へのアクセスを扱う', async ({ page, session }) => {
  await openProject(session);
  const unauthenticated = await request(session, '/api/state', { token: false });
  expect([401, 403]).toContain(unauthenticated.status);
  const wrongOrigin = await request(session, '/api/state', { headers: { Origin: 'https://example.invalid' } });
  expect(wrongOrigin.status).toBe(403);
  const outside = path.join(session.temp, 'outside.tex');
  await writeFile(outside, 'outside-file-must-not-be-served');
  const traversal = await request(session, `/api/file?${new URLSearchParams({ path: '../outside.tex' })}`);
  expect([400, 403, 404]).toContain(traversal.status);
  expect(await traversal.text()).not.toContain('outside-file-must-not-be-served');

  const original = await json(session, '/api/file?path=main.tex');
  const lineFile = path.join(session.project, 'line-endings.tex');
  await writeFile(lineFile, '\ufefffirst\r\nsecond\r\n', 'utf8');
  const lineDocument = await json(session, '/api/file?path=line-endings.tex');
  expect(lineDocument.lineEnding).toBe('crlf');
  await json(session, '/api/file', {
    method: 'PUT', data: { path: 'line-endings.tex', revision: lineDocument.revision, content: 'first\nchanged\n' },
  });
  expect(await readFile(lineFile, 'utf8')).toBe('\ufefffirst\r\nchanged\r\n');

  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  const editor = page.getByTestId('editor').locator('.cm-content');
  await expect(editor).toContainText('Local TeX Editor');
  await editor.click();
  await page.keyboard.press('Control+Home');
  await page.keyboard.insertText('% 編集中の内容\n');
  await writeFile(path.join(session.project, 'main.tex'), original.content + '\n% 外部の変更\n', 'utf8');
  const saveResponse = page.waitForResponse(response =>
    new URL(response.url()).pathname === '/api/file' && response.request().method() === 'PUT');
  await page.getByTestId('save-button').click();
  expect((await saveResponse).status()).toBe(409);
  await expect(page.getByTestId('app-error')).toBeVisible();
  await expect(editor).toContainText('編集中の内容');
  const onDisk = await readFile(path.join(session.project, 'main.tex'), 'utf8');
  expect(onDisk).toContain('外部の変更');
  expect(onDisk).not.toContain('編集中の内容');
});

test('TeX の失敗を画面に出し，実行中のコンパイルを停止する', async ({ page, session }) => {
  await openProject(session);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  const source = await readFile(path.join(session.project, 'main.tex'), 'utf8');
  await expect(page.getByTestId('editor').locator('.cm-content')).toContainText('Local TeX Editor');
  const success = await compileFromUI(page, session);
  expect(success.finished.status, success.finished.log).toBe('success');
  const previousPDF = Buffer.from(await (await request(session, '/api/pdf')).arrayBuffer());

  await replaceEditor(page, source.replace('\\end{document}', '\\NoSuchControlSequenceForEditorTest\n\\end{document}'));
  const failure = await compileFromUI(page, session);
  expect(failure.finished.status, failure.finished.log).toBe('failed');
  expect(failure.finished.exitCode).not.toBe(0);
  expect(failure.finished.log).toMatch(/Undefined control sequence/);
  expect(failure.finished.error.length).toBeGreaterThan(0);
  expect(failure.finished.pdfReady).toBe(true);
  expect(failure.finished.pdfBuildId).toBe(success.finished.pdfBuildId);
  const afterFailure = Buffer.from(await (await request(session, '/api/pdf')).arrayBuffer());
  expect(afterFailure.equals(previousPDF)).toBe(true);
  await expect(page.getByTestId('build-status')).toHaveAttribute('data-status', 'failed');
  await expect(page.getByTestId('build-log')).toContainText('Undefined control sequence');

  const slowSource = String.raw`\documentclass[uplatex,dvipdfmx]{jsarticle}
\begin{document}
\typeout{LOCAL_TEX_EDITOR_LONG_COMPILE}
\newcount\delaycounter
\delaycounter=0
\loop\advance\delaycounter by 1\ifnum\delaycounter<1000000000\repeat
Finished.
\end{document}
`;
  await replaceEditor(page, slowSource);
  const runningResponse = page.waitForResponse(response =>
    new URL(response.url()).pathname === '/api/build' && response.request().method() === 'POST');
  await page.getByTestId('compile-button').click();
  const started = await (await runningResponse).json();
  await waitUntil(() => json(session, '/api/build'), build =>
    build?.id === started.id && build.status === 'running' && build.log.includes('LOCAL_TEX_EDITOR_LONG_COMPILE'),
  20_000, 'The long-running TeX process did not start.');
  await page.getByTestId('stop-button').click();
  const stopped = await buildFinished(session, started.id);
  expect(stopped.status, stopped.log + '\n' + stopped.error).toBe('cancelled');
  await expect(page.getByTestId('build-status')).toHaveAttribute('data-status', 'cancelled');

  await replaceEditor(page, source);
  const recovered = await compileFromUI(page, session);
  expect(recovered.finished.status, recovered.finished.log + '\n' + recovered.finished.error).toBe('success');
});
