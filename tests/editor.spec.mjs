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

async function setAutoSave(page, enabled) {
  await page.getByTestId('settings-button').click();
  await page.getByTestId('autosave-setting').setChecked(enabled);
  await page.locator('#save-settings').click();
  await expect(page.getByTestId('autosave-setting')).not.toBeVisible();
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
    let processHandle;
    const tokens = [];
    async function start() {
      await rm(readyFile, { force: true });
      startupError = undefined;
      processHandle = spawn(executable, args, {
        cwd: repository,
        windowsHide: true,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      processHandle.on('error', error => { startupError = error; });
      processHandle.stdout.on('data', chunk => { stdout += chunk.toString('utf8'); });
      processHandle.stderr.on('data', chunk => { stderr += chunk.toString('utf8'); });
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
      tokens.push(ready.token);
      return ready;
    }
    async function stop() {
      if (!processHandle) return;
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
    }
    try {
      session = { ...await start(), project, temp };
      session.restart = async () => {
        await stop();
        Object.assign(session, await start());
      };
      await use(session);
    } finally {
      await stop();
      if (testInfo.status !== testInfo.expectedStatus) {
        await testInfo.attach('application-output.txt', {
          body: Buffer.from(tokens.reduce((output, token) => redactOutput(output, token), `${stdout}\n${stderr}`)),
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

  const firstPage = page.locator('.pdfViewer .page[data-page-number="1"]');
  const secondPage = page.locator('.pdfViewer .page[data-page-number="2"]');
  await expect(firstPage).toBeAttached();
  await expect(secondPage).toBeAttached();

  await page.getByTestId('pdf-search-toggle').click();
  await page.getByTestId('pdf-search-input').fill('二ページ目');
  await page.getByTestId('pdf-search-input').press('Enter');
  await expect(page.getByTestId('pdf-search-count')).toContainText('1');
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  await page.getByTestId('pdf-search-input').press('Escape');
  await page.getByTestId('pdf-prev').click();
  await expect(page.getByTestId('pdf-page')).toHaveValue('1');

  const pdfScroll = page.getByTestId('pdf-scroll');
  await pdfScroll.hover();
  await page.mouse.wheel(0, 1600);
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  await expect(canvas).toHaveAttribute('data-page', '2');
  await page.getByTestId('pdf-prev').click();
  await expect(page.getByTestId('pdf-page')).toHaveValue('1');

  const initialWidth = await firstPage.evaluate(element => element.getBoundingClientRect().width);
  await page.getByTestId('pdf-zoom-in').click();
  await expect.poll(() => firstPage.evaluate(element => element.getBoundingClientRect().width)).toBeGreaterThan(initialWidth);
  await page.getByTestId('pdf-zoom-out').click();
  await page.getByTestId('pdf-zoom').selectOption('page-width');
  await expect(canvas).toHaveAttribute('data-ready', 'true');

  const themeButton = page.getByTestId('theme-toggle');
  const wasDark = (await themeButton.getAttribute('aria-pressed')) === 'true';
  const initialBackground = await page.locator('.app-header').evaluate(element => getComputedStyle(element).backgroundColor);
  await themeButton.click();
  await expect(themeButton).toHaveAttribute('aria-pressed', String(!wasDark));
  await expect.poll(() => page.locator('.app-header').evaluate(element => getComputedStyle(element).backgroundColor)).not.toBe(initialBackground);
  await page.screenshot({ path: testInfo.outputPath('editor-alternate-theme.png') });
  await themeButton.click();
  await expect(themeButton).toHaveAttribute('aria-pressed', String(wasDark));

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

test('日本語のファイル名で原稿と PDF の位置を行き来する', async ({ page, session }) => {
  const sourcePath = '追加の 原稿.tex';
  await openProject(session);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(page.getByTestId('editor').locator('.cm-content')).toContainText('Local TeX Editor');
  const { finished } = await compileFromUI(page, session);
  expect(finished.status, finished.log + '\n' + finished.error).toBe('success');
  const canvas = page.getByTestId('pdf-canvas');
  await expect(canvas).toHaveAttribute('data-ready', 'true');

  const forward = await json(session, '/api/synctex/forward', {
    method: 'POST', data: { buildId: finished.pdfBuildId, path: sourcePath, line: 2, column: 1 },
  });
  expect(forward.buildId).toBe(finished.pdfBuildId);
  expect(forward.sourceChanged).toBe(false);
  expect(forward.locations.length).toBeGreaterThan(0);
  const location = forward.locations.find(item => item.page === 2);
  expect(location).toBeDefined();
  expect(location.width).toBeGreaterThan(0);
  expect(location.height).toBeGreaterThan(0);
  const inverse = await json(session, '/api/synctex/inverse', {
    method: 'POST',
    data: {
      buildId: finished.pdfBuildId, page: location.page,
      x: location.x + Math.min(location.width / 2, 20), y: location.y + location.height / 2,
    },
  });
  expect(inverse.path).toBe(sourcePath);
  expect(inverse.line).toBeGreaterThanOrEqual(2);
  expect(inverse.line).toBeLessThanOrEqual(3);
  expect(inverse.sourceChanged).toBe(false);

  await page.getByTestId(`file-${sourcePath}`).click();
  const editor = page.getByTestId('editor').locator('.cm-content');
  await expect(editor).toContainText('これは SyncTeX');
  await editor.click();
  await page.keyboard.press('Control+Home');
  await page.keyboard.press('ArrowDown');
  const forwardResponse = page.waitForResponse(response =>
    new URL(response.url()).pathname === '/api/synctex/forward' && response.request().method() === 'POST');
  await page.getByTestId('synctex-forward').click();
  expect((await forwardResponse).ok()).toBe(true);
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  await expect(canvas).toHaveAttribute('data-page', '2');

  // Open another source file first, so that the click proves the inverse query
  // navigates to the included file instead of leaving an existing editor open.
  await page.getByTestId('file-main.tex').click();
  await expect(editor).toContainText('Local TeX Editor');
  const inverseResponse = page.waitForResponse(response =>
    new URL(response.url()).pathname === '/api/synctex/inverse' && response.request().method() === 'POST');
  const referenceText = page.locator('.pdfViewer .page[data-page-number="2"] .textLayer span')
    .filter({ hasText: 'SyncTeX' }).first();
  await referenceText.dblclick();
  const clicked = await inverseResponse;
  expect(clicked.ok(), await clicked.text()).toBe(true);
  const clickedSource = await clicked.json();
  expect(clickedSource.path).toBe(sourcePath);
  await expect(editor).toContainText('これは SyncTeX');
  await expect(page.getByTestId('editor')).toHaveAttribute('data-path', sourcePath);
  await expect(page.getByTestId('editor')).toHaveAttribute('data-line', String(clickedSource.line));
  await expect(page.getByTestId('app-error')).not.toBeVisible();

  await session.restart();
  const restored = await json(session, '/api/build');
  expect(restored.restored).toBe(true);
  expect(restored.pdfReady).toBe(true);
  expect(restored.synctexReady).toBe(true);
  expect(restored.pdfBuildId).toBe(finished.pdfBuildId);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(canvas).toHaveAttribute('data-ready', 'true');
  await expect(canvas).toHaveAttribute('data-build-id', finished.pdfBuildId);
  const restoredForward = await json(session, '/api/synctex/forward', {
    method: 'POST', data: { buildId: finished.pdfBuildId, path: sourcePath, line: 2, column: 1 },
  });
  expect(restoredForward.locations.some(item => item.page === 2)).toBe(true);
});

test('自動保存，補完，アウトラインを使い，草稿を再起動後に戻す', async ({ page, session }) => {
  page.on('dialog', dialog => dialog.accept());
  await openProject(session);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  const editor = page.getByTestId('editor').locator('.cm-content');
  await expect(editor).toContainText('Local TeX Editor');
  const autoSavedComment = '% 自動保存でディスクへ書き込む\n';
  await editor.click();
  await page.keyboard.press('Control+Home');
  await page.keyboard.insertText(autoSavedComment);
  await expect.poll(() => readFile(path.join(session.project, 'main.tex'), 'utf8')).toContain(autoSavedComment.trim());

  await page.getByTestId('outline-tab').click();
  const section = page.getByTestId('outline-tree').locator('button[data-path="main.tex"]')
    .filter({ hasText: '二ページ目' }).first();
  await expect(section).toBeVisible();
  const currentSource = await readFile(path.join(session.project, 'main.tex'), 'utf8');
  const sectionLine = String(currentSource.split('\n').findIndex(line => line.includes('\\section{二ページ目}')) + 1);
  await expect(section).toHaveAttribute('data-line', sectionLine);
  await section.click();
  await expect(page.getByTestId('editor')).toHaveAttribute('data-line', sectionLine);

  await setAutoSave(page, false);
  await editor.click();
  await page.keyboard.press('Control+End');
  await page.keyboard.insertText('\n\\cite{sam');
  await page.keyboard.press('Control+Space');
  await page.getByRole('option', { name: /sample2026/ }).click();
  await page.keyboard.insertText('}');
  await expect(editor).toContainText('\\cite{sample2026}');

  const draftComment = '% 起動し直した後に草稿から戻す\n';
  await page.keyboard.press('Control+Home');
  await page.keyboard.insertText(draftComment);
  await expect(page.getByTestId('draft-status')).toHaveAttribute('data-status', 'saved');
  const current = await json(session, '/api/state');
  const storageURL = `/api/storage?${new URLSearchParams({ key: `drafts:${current.project.root}` })}`;
  let savedDraft;
  await expect.poll(async () => {
    const stored = await json(session, storageURL);
    savedDraft = stored.value?.entries?.find(entry => entry.path === 'main.tex' && entry.content.includes(draftComment.trim()));
    return Boolean(savedDraft);
  }).toBe(true);
  expect(await readFile(path.join(session.project, 'main.tex'), 'utf8')).not.toContain(draftComment.trim());

  await session.restart();
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await page.getByTestId('review-drafts').click();
  await expect(page.getByTestId('recovery-dialog')).toBeVisible();
  await page.getByTestId('recovery-file').selectOption(savedDraft.id);
  await page.getByTestId('restore-draft').click();
  await expect(editor).toContainText(draftComment.trim());
  expect(await readFile(path.join(session.project, 'main.tex'), 'utf8')).not.toContain(draftComment.trim());
  await setAutoSave(page, true);
  await expect.poll(() => readFile(path.join(session.project, 'main.tex'), 'utf8')).toContain(draftComment.trim());
});

test('150 ページの PDF の末尾へ移動し，必要なページを描画する', async ({ page, session }, testInfo) => {
  await openProject(session);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(page.getByTestId('editor').locator('.cm-content')).toContainText('Local TeX Editor');
  const source = String.raw`\documentclass[uplatex,dvipdfmx]{jsarticle}
\begin{document}
\newcount\editorpagenumber
\editorpagenumber=1
\loop
\section*{ページ \the\editorpagenumber}
長い原稿をブラウザで読み，必要なページを表示する．
\ifnum\editorpagenumber<150\newpage\fi
\advance\editorpagenumber by 1
\ifnum\editorpagenumber<151\repeat
\end{document}
`;
  await replaceEditor(page, source);
  const { finished } = await compileFromUI(page, session);
  expect(finished.status, finished.log + '\n' + finished.error).toBe('success');
  const canvas = page.getByTestId('pdf-canvas');
  await expect(canvas).toHaveAttribute('data-ready', 'true');
  await expect(page.locator('#pdf-pages')).toHaveText('150');
  await page.getByTestId('pdf-page').fill('150');
  await page.getByTestId('pdf-page').press('Enter');
  await expect(canvas).toHaveAttribute('data-page', '150');
  await expect(canvas).toHaveAttribute('data-ready', 'true');
  await expect(canvas).toHaveAttribute('data-build-id', finished.pdfBuildId);
  const rendering = await page.locator('.pdfViewer').evaluate(viewer => {
    const canvases = [...viewer.querySelectorAll('canvas')];
    return {
      pages: viewer.querySelectorAll('.page').length,
      canvases: canvases.length,
      pixels: canvases.reduce((sum, item) => sum + item.width * item.height, 0),
    };
  });
  expect(rendering.pages).toBe(150);
  expect(rendering.canvases).toBeGreaterThan(0);
  expect(rendering.canvases).toBeLessThanOrEqual(20);
  await testInfo.attach('pdf-rendering.json', {
    body: Buffer.from(JSON.stringify({ ...rendering, elapsedMS: finished.elapsedMs }, null, 2)),
    contentType: 'application/json',
  });
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
  await setAutoSave(page, false);
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

  const failingSource = source.replace('\\end{document}', '\\NoSuchControlSequenceForEditorTest\n\\end{document}');
  const failingLine = failingSource.split('\n').findIndex(line => line.includes('\\NoSuchControlSequenceForEditorTest')) + 1;
  await replaceEditor(page, failingSource);
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
  const errorLocation = page.getByTestId('log-locations').locator(`button[data-path="main.tex"][data-line="${failingLine}"]`).first();
  await errorLocation.click();
  await expect(page.getByTestId('editor')).toHaveAttribute('data-path', 'main.tex');
  await expect(page.getByTestId('editor')).toHaveAttribute('data-line', String(failingLine));

  const slowSource = String.raw`\documentclass[uplatex,dvipdfmx]{jsarticle}
\begin{document}
\typeout{LOCAL_TEX_EDITOR_LONG_COMPILE}
\newwrite\editorreadyfile
\immediate\openout\editorreadyfile=editor-long-compile.ready
\immediate\write\editorreadyfile{LOCAL_TEX_EDITOR_LONG_COMPILE}
\immediate\closeout\editorreadyfile
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
  // Windows TeX may buffer terminal output during a long calculation. A closed
  // file confirms that the actual TeX child reached the loop before we stop it.
  const markerPath = path.join(session.project, '.local-tex-editor', 'build', 'editor-long-compile.ready');
  await waitUntil(async () => {
    const build = await json(session, '/api/build');
    const marker = await readFile(markerPath, 'utf8').catch(error => {
      if (error.code === 'ENOENT') return '';
      throw error;
    });
    return { build, marker };
  }, value => value.build?.id === started.id && value.build.status === 'running' &&
    value.marker.includes('LOCAL_TEX_EDITOR_LONG_COMPILE'),
  20_000, 'The long-running TeX process did not reach the loop.');
  await page.getByTestId('stop-button').click();
  const stopped = await buildFinished(session, started.id);
  expect(stopped.status, stopped.log + '\n' + stopped.error).toBe('cancelled');
  await expect(page.getByTestId('build-status')).toHaveAttribute('data-status', 'cancelled');

  await replaceEditor(page, source);
  const recovered = await compileFromUI(page, session);
  expect(recovered.finished.status, recovered.finished.log + '\n' + recovered.finished.error).toBe('success');
});
