import { test as base, expect } from '@playwright/test';
import { build as bundle } from 'esbuild';
import { execFile, spawn } from 'node:child_process';
import { once } from 'node:events';
import { cp, mkdir, mkdtemp, readFile, readdir, rm, stat, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { promisify } from 'node:util';

const repository = path.resolve(import.meta.dirname, '..');
const fixture = path.join(import.meta.dirname, 'fixtures', 'japanese');
const executable = path.resolve(process.env.LOCAL_TEX_EDITOR_TEST_EXE ||
  path.join(repository, 'bin', process.platform === 'win32' ? 'local-tex-editor.exe' : 'local-tex-editor'));
const execFileAsync = promisify(execFile);

function exampleFigurePDF() {
  const content = 'BT /F1 18 Tf 30 100 Td (Figure PDF) Tj ET\n';
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>',
    `<< /Length ${Buffer.byteLength(content)} >>\nstream\n${content}endstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ];
  return pdfFromObjects(objects);
}

function pdfFromObjects(objects) {
  let pdf = '%PDF-1.7\n';
  const offsets = [0];
  for (const [index, object] of objects.entries()) {
    offsets.push(Buffer.byteLength(pdf));
    pdf += `${index + 1} 0 obj\n${object}\nendobj\n`;
  }
  const xref = Buffer.byteLength(pdf);
  pdf += `xref\n0 ${offsets.length}\n0000000000 65535 f \n`;
  for (const offset of offsets.slice(1)) pdf += `${String(offset).padStart(10, '0')} 00000 n \n`;
  pdf += `trailer\n<< /Size ${offsets.length} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(pdf);
}

function pdfWithRotatedPages() {
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R 5 0 R 7 0 R 9 0 R] /Count 4 >>',
  ];
  for (const rotation of [0, 90, 180, 270]) {
    // The filled rectangle starts 40 points from the page's left edge
    // and 70 points from its top edge, with a width of 80 and a height of 18.
    const content = '0 g 40 312 80 18 re f\n';
    const contentObject = objects.length + 2;
    objects.push(
      `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 400] /Rotate ${rotation} /Resources << >> /Contents ${contentObject} 0 R >>`,
      `<< /Length ${Buffer.byteLength(content)} >>\nstream\n${content}endstream`,
    );
  }
  return pdfFromObjects(objects);
}

async function openPDFViewer(page, pdf) {
  const source = await readFile(path.join(repository, 'web/src/index.html'), 'utf8');
  const pane = source.match(/<section class="pdf-pane"[\s\S]*?<\/section>/)?.[0];
  expect(pane).toBeTruthy();
  const script = await bundle({
    stdin: {
      contents: `import { PDFViewer } from './pdf-viewer.js';
        window.viewerErrors = [];
        window.inverseLocations = [];
        window.testViewer = new PDFViewer({ token: 'test-token',
          onError: error => window.viewerErrors.push(error.message),
          onInverse: location => window.inverseLocations.push(location) });
        await window.testViewer.load('test-build', 'test-project');`,
      resolveDir: path.join(repository, 'web/src'),
    },
    bundle: true, format: 'esm', write: false,
  });
  const assets = new Map([
    ['/viewer.js', { contentType: 'text/javascript', body: script.outputFiles[0].text }],
    ['/pdf.worker.min.mjs', { contentType: 'text/javascript', body: await readFile(path.join(repository, 'node_modules/pdfjs-dist/build/pdf.worker.min.mjs')) }],
    ['/pdf_viewer.css', { contentType: 'text/css', body: await readFile(path.join(repository, 'node_modules/pdfjs-dist/web/pdf_viewer.css')) }],
    ['/style.css', { contentType: 'text/css', body: await readFile(path.join(repository, 'web/src/style.css')) }],
    ['/pdf-search.css', { contentType: 'text/css', body: await readFile(path.join(repository, 'web/src/pdf-search.css')) }],
    ['/api/pdf', { contentType: 'application/pdf', body: pdf }],
    ['/', { contentType: 'text/html', body: `<!doctype html><html><head>
      <link rel="stylesheet" href="/pdf_viewer.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/pdf-search.css">
      <style>#pdf-pane { width: 900px; height: 900px; }</style>
      </head><body>${pane}<script type="module" src="/viewer.js"></script></body></html>` }],
  ]);
  const origin = 'http://127.0.0.1:41537';
  await page.route(`${origin}/**`, async route => {
    const asset = assets.get(new URL(route.request().url()).pathname);
    if (asset) await route.fulfill(asset);
    else await route.fulfill({ status: 404, body: '' });
  });
  await page.goto(origin);
  await expect(page.getByTestId('pdf-canvas')).toHaveAttribute('data-ready', 'true');
}

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

function expectedMainContent(session, content) {
  return content.replaceAll('\r\n', '\n').replaceAll('\n', session.mainLineEnding);
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

async function attachBuildFiles(testInfo, directory, prefix, recursive = false) {
  const entries = await readdir(directory, { withFileTypes: true }).catch(error => {
    if (error.code === 'ENOENT') return [];
    throw error;
  });
  for (const entry of entries) {
    const file = path.join(directory, entry.name);
    if (recursive && entry.isDirectory()) {
      await attachBuildFiles(testInfo, file, `${prefix}-${entry.name}`, true);
    } else if (entry.isFile() && (/(?:\.fls|\.synctex(?:\.gz)?)$/.test(entry.name) || /^pdf-.*\.json$/.test(entry.name))) {
      await testInfo.attach(`${prefix}-${entry.name}`, {
        path: file,
        contentType: entry.name.endsWith('.gz') ? 'application/gzip' : entry.name.endsWith('.json') ? 'application/json' : 'text/plain',
      });
    }
  }
}

async function recordEmptySyncTeX(testInfo, session, buildId, forward) {
  const result = { includedFile: forward, mainFile: null, cli: [] };
  result.mainFile = await json(session, '/api/synctex/forward', {
    method: 'POST', data: { buildId, path: 'main.tex', line: 10, column: 1 },
  }).catch(error => ({ error: error.message }));
  const cacheRoot = path.join(session.temp, 'config', 'cache');
  const directories = await readdir(cacheRoot, { withFileTypes: true }).catch(() => []);
  for (const directory of directories.filter(entry => entry.isDirectory())) {
    const cwd = path.join(cacheRoot, directory.name);
    const metadata = await readFile(path.join(cwd, `pdf-${buildId}.json`), 'utf8')
      .then(JSON.parse, () => null);
    if (!metadata?.synctex?.bin) continue;
    const program = path.join(metadata.synctex.bin, process.platform === 'win32' ? 'synctex.exe' : 'synctex');
    const environment = Object.fromEntries(Object.entries(process.env)
      .filter(([key]) => !/^SYNCTEX_(?:VIEWER|EDITOR|BUILD_DIRECTORY)$/i.test(key)));
    for (const input of metadata.synctex.inputs || []) {
      if (!['main.tex', '追加の 原稿.tex'].includes(input.path)) continue;
      const line = input.path === 'main.tex' ? 10 : 2;
      const args = ['view', '-i', `${line}:1:${input.alias}`, '-o', `pdf-${buildId}.pdf`];
      const output = await execFileAsync(program, args, {
        cwd, windowsHide: true, timeout: 10_000, env: environment,
      }).then(value => ({ ...value, exitCode: 0 }), error => ({
        stdout: error.stdout, stderr: error.stderr, exitCode: error.code, message: error.message,
      }));
      result.cli.push({ source: input.path, args, ...output });
    }
  }
  const file = testInfo.outputPath('synctex-empty-result.json');
  await writeFile(file, JSON.stringify(result, null, 2));
  await testInfo.attach('synctex-empty-result.json', { path: file, contentType: 'application/json' });
}

const test = base.extend({
  session: async ({}, use, testInfo) => {
    await stat(executable).catch(() => {
      throw new Error(`Build the application before running browser tests: ${executable}`);
    });
    const temp = await mkdtemp(path.join(os.tmpdir(), 'ltex-'));
    const project = path.join(temp, '日本語の原稿 with spaces');
    await cp(fixture, project, { recursive: true });
    const mainLineEnding = (await readFile(path.join(project, 'main.tex'), 'utf8')).includes('\r\n') ? '\r\n' : '\n';
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
      session = { ...await start(), project, temp, mainLineEnding };
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
        await attachBuildFiles(testInfo, path.join(project, '.local-tex-editor', 'build'), 'tex');
        await attachBuildFiles(testInfo, path.join(temp, 'config', 'cache'), 'cache', true);
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

test('終了を取り消して編集を続け，保存してからタブを閉じる', async ({ page, session }) => {
  await openProject(session);
  const opened = page.waitForEvent('popup');
  await page.evaluate(url => { window.open(url, 'local-tex-editor'); }, `${session.url}#token=${encodeURIComponent(session.token)}`);
  const editorPage = await opened;
  await expect(editorPage.getByTestId('editor').locator('.cm-content')).toBeVisible();
  await setAutoSave(editorPage, false);
  const content = '% 終了する前に保存する原稿\n\\newcommand{\\example}{saved}\n';
  await replaceEditor(editorPage, content);
  await editorPage.locator('#shutdown-button').click();
  await expect(editorPage.locator('#decision-dialog')).toBeVisible();
  await editorPage.locator('#decision-cancel').click();
  await expect(editorPage.locator('#decision-dialog')).not.toBeVisible();
  await expect(editorPage.getByTestId('editor').locator('.cm-content')).toContainText('終了する前に保存する原稿');
  expect((await json(session, '/api/state')).project).not.toBeNull();

  await editorPage.locator('#shutdown-button').click();
  const closed = editorPage.waitForEvent('close');
  await editorPage.locator('#decision-primary').click();
  await closed;
  expect(editorPage.isClosed()).toBe(true);
  expect(await readFile(path.join(session.project, 'main.tex'), 'utf8')).toBe(expectedMainContent(session, content));
});

test('終了確認中に別のファイルが読み込まれても，取消後に元の原稿を編集できる', async ({ page, session }) => {
  await writeFile(path.join(session.project, 'notes.txt'), '読み込みを待つファイル\n', 'utf8');
  await openProject(session);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(page.getByTestId('editor')).toHaveAttribute('data-path', 'main.tex');
  await setAutoSave(page, false);
  const content = '% 終了を取り消した後も編集する原稿\n';
  await replaceEditor(page, content);

  let release;
  const responseGate = new Promise(resolve => { release = resolve; });
  let requested;
  const requestStarted = new Promise(resolve => { requested = resolve; });
  const fileURL = url => url.pathname === '/api/file' && url.searchParams.get('path') === 'notes.txt';
  await page.route(fileURL, async route => {
    const response = await route.fetch();
    requested();
    await responseGate;
    await route.fulfill({ response });
  });
  try {
    await page.getByTestId('file-notes.txt').click();
    await requestStarted;
    await page.locator('#shutdown-button').click();
    await expect(page.locator('#decision-dialog')).toBeVisible();
    release();
    await expect(page.getByTestId('editor')).toHaveAttribute('data-path', 'notes.txt');
    await expect(page.getByTestId('editor').locator('.cm-content')).toHaveAttribute('contenteditable', 'false');
    await page.locator('#decision-cancel').click();
    await expect(page.locator('#decision-dialog')).not.toBeVisible();
    await expect(page.getByTestId('editor').locator('.cm-content')).toHaveAttribute('contenteditable', 'true');

    await page.locator('#editor-tabs .tab-select[data-path="main.tex"]').click();
    const editor = page.getByTestId('editor').locator('.cm-content');
    await expect(editor).toHaveAttribute('contenteditable', 'true');
    await expect(editor).toContainText('終了を取り消した後も編集する原稿');
    await editor.click();
    await page.keyboard.press('Control+End');
    await page.keyboard.insertText('% 続けて入力できた\n');
    await expect(editor).toContainText('続けて入力できた');
    await page.keyboard.press('Control+S');
    await expect.poll(() => readFile(path.join(session.project, 'main.tex'), 'utf8')).toBe(expectedMainContent(session, content + '% 続けて入力できた\n'));
  } finally {
    release();
    await page.unroute(fileURL);
  }
});

test('自動で閉じられないタブに終了を表示し，草稿を次回に戻す', async ({ page, session }, testInfo) => {
  await openProject(session);
  const original = await readFile(path.join(session.project, 'main.tex'), 'utf8');
  await page.addInitScript(() => {
    window.__closeRequests = 0;
    window.close = () => { window.__closeRequests++; };
  });
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(page.getByTestId('editor').locator('.cm-content')).toBeVisible();
  await setAutoSave(page, false);
  const draft = '% 終了後に草稿から戻す内容\n' + original;
  await replaceEditor(page, draft);
  await page.locator('#shutdown-button').click();
  await page.locator('#decision-secondary').click();
  await expect(page.getByTestId('closed-screen')).toBeVisible();
  await expect(page.locator('#closed-message')).toContainText('Ctrl+W');
  await expect(page.getByTestId('app-toolbar')).not.toBeVisible();
  await expect(page.locator('#workbench')).not.toBeVisible();
  expect(await page.evaluate(() => window.__closeRequests)).toBe(1);
  expect(await readFile(path.join(session.project, 'main.tex'), 'utf8')).toBe(original);
  await page.screenshot({ path: testInfo.outputPath('closed-tab.png') });

  await session.restart();
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await page.getByTestId('review-drafts').click();
  await page.getByTestId('restore-draft').click();
  await expect(page.getByTestId('editor').locator('.cm-content')).toContainText('終了後に草稿から戻す内容');
});

test('改行を貼り付けて検索し，TeX のコマンドをそのまま置換する', async ({ page, session }, testInfo) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await openProject(session);
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(page.getByTestId('editor').locator('.cm-content')).toBeVisible();
  await setAutoSave(page, false);
  const block = String.raw`\ifneuripslayout
  \input{common-neurips-uplatex}
\else
  \input{common-uplatex}
\fi`;
  const replacement = String.raw`\newcommand{\test}{first}
\textbf{second} \\ third`;
  await replaceEditor(page, `% before\n${block}\n% middle\n${block}\n% after\n`);
  await page.locator('#find-button').click();
  const search = page.getByTestId('source-search-input');
  await expect(search).toBeFocused();
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.evaluate(text => navigator.clipboard.writeText(text), block.replaceAll('\n', '\r\n'));
  await search.press('Control+V');
  await expect(search).toHaveValue(block);
  await expect(page.getByTestId('source-search-count')).toHaveText('2 件');
  await search.press('Enter');
  await expect(page.getByTestId('source-search-count')).toHaveText('1 / 2');
  await search.press('Enter');
  await expect(page.getByTestId('source-search-count')).toHaveText('2 / 2');
  await search.press('Shift+Enter');
  await expect(page.getByTestId('source-search-count')).toHaveText('1 / 2');
  await page.getByTestId('source-replace-input').fill(replacement);
  await page.getByTestId('source-search-replace-all').click();
  await expect(page.getByTestId('source-search-count')).toHaveText('0 件');
  await page.getByTestId('save-button').click();
  await expect.poll(() => readFile(path.join(session.project, 'main.tex'), 'utf8')).toBe(expectedMainContent(session, `% before\n${replacement}\n% middle\n${replacement}\n% after\n`));

  let builds = 0;
  page.on('request', request => { if (new URL(request.url()).pathname === '/api/build' && request.method() === 'POST') builds++; });
  await search.fill('first');
  await search.press('End');
  await search.press('Control+Enter');
  await search.pressSequentially('second');
  await expect(search).toHaveValue('first\nsecond');
  expect(builds).toBe(0);
  await search.fill('[');
  await page.getByRole('checkbox', { name: '正規表現', exact: true }).check();
  await expect(page.getByTestId('source-search-count')).toHaveText('正規表現を確認');
  await expect(page.getByTestId('source-search-next')).toBeDisabled();
  await page.getByRole('checkbox', { name: '正規表現', exact: true }).uncheck();
  await search.fill(replacement);
  await expect(page.getByTestId('source-search-count')).toHaveText('1 / 2');
  await page.screenshot({ path: testInfo.outputPath('multiline-search.png') });
  await search.press('Escape');
  await expect(search).not.toBeVisible();
  expect(errors).toEqual([]);
});

test('フォルダーから画像と PDF を開き，編集中の原稿へ戻る', async ({ page, session }, testInfo) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const figures = path.join(session.project, 'figures');
  await mkdir(path.join(figures, 'empty'), { recursive: true });
  const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQaXjwHwAE1AKEf31AsgAAAABJRU5ErkJggg==', 'base64');
  await writeFile(path.join(figures, '図.png'), png);
  await writeFile(path.join(figures, 'plot.pdf'), exampleFigurePDF());
  await writeFile(path.join(figures, 'shape.svg'), '<svg xmlns="http://www.w3.org/2000/svg" width="320" height="160"><rect width="320" height="160" fill="#137b72"/><text x="30" y="85" fill="white" font-size="28">Figure</text></svg>');
  const metadata = await openProject(session);
  expect(metadata.project.directories).toEqual(expect.arrayContaining(['figures', 'figures/empty']));
  expect(metadata.project.files.find(file => file.path === 'figures/図.png')?.kind).toBe('image');
  expect(metadata.project.files.find(file => file.path === 'figures/plot.pdf')?.kind).toBe('pdf');
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  await expect(page.getByTestId('editor').locator('.cm-content')).toBeVisible();
  await setAutoSave(page, false);
  const source = '% 図を確認する前の草稿\n\\newcommand{\\example}{draft}\n';
  await replaceEditor(page, source);
  await page.keyboard.press('Control+Home');
  await page.keyboard.press('ArrowDown');
  const line = await page.getByTestId('editor').getAttribute('data-line');
  const folder = page.locator('[data-testid="tree-folder"][data-path="figures"]');
  await folder.locator(':scope > summary').click();
  await expect(page.locator('[data-testid="tree-folder"][data-path="figures/empty"]')).toBeVisible();
  await page.getByTestId('file-figures/図.png').click();
  const assetImage = page.locator('#asset-preview img');
  await expect(assetImage).toBeVisible();
  await expect.poll(() => assetImage.evaluate(image => image.complete && image.naturalWidth)).toBe(1);
  await expect(page.getByTestId('save-button')).toBeDisabled();
  await page.getByTestId('file-figures/shape.svg').click();
  await expect.poll(() => assetImage.evaluate(image => image.complete && image.naturalWidth)).toBe(320);
  await page.screenshot({ path: testInfo.outputPath('image-preview.png') });
  await page.getByTestId('file-figures/plot.pdf').click();
  await expect(page.locator('#asset-preview canvas').first()).toBeVisible();
  await expect.poll(() => page.locator('#asset-preview canvas').first().evaluate(canvas => canvas.width)).toBeGreaterThan(100);
  await page.locator('#editor-tabs [data-path="main.tex"]').click();
  await expect(page.getByTestId('editor')).toBeVisible();
  await expect(page.getByTestId('editor')).toHaveAttribute('data-line', line);
  await expect(page.getByTestId('editor').locator('.cm-content')).toContainText('図を確認する前の草稿');
  await expect(page.getByTestId('save-button')).toBeEnabled();
  await page.getByTestId('save-button').click();
  await expect.poll(() => readFile(path.join(session.project, 'main.tex'), 'utf8')).toBe(expectedMainContent(session, source));

  await page.setViewportSize({ width: 1440, height: 900 });
  const dimensions = await page.evaluate(() => ({
    toolbar: document.querySelector('.app-header').getBoundingClientRect().height,
    editorTop: document.querySelector('#editor').getBoundingClientRect().top,
    pdfTop: document.querySelector('#pdf-scroll').getBoundingClientRect().top,
  }));
  expect(dimensions.toolbar).toBeLessThanOrEqual(48);
  expect(dimensions.editorTop).toBeLessThanOrEqual(100);
  expect(dimensions.pdfTop).toBeLessThanOrEqual(100);
  await page.screenshot({ path: testInfo.outputPath('compact-editor.png') });
  expect(errors).toEqual([]);
});

test('日本語の原稿を編集し，参考文献と PDF を更新する', async ({ page, session }, testInfo) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(`${session.url}#token=${encodeURIComponent(session.token)}`);
  if (!await page.getByTestId('project-path').isVisible()) {
    await page.locator('#welcome-open').click();
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
  const expandedWidth = await firstPage.evaluate(element => element.getBoundingClientRect().width);
  await page.getByTestId('pdf-zoom-out').click();
  await expect.poll(() => firstPage.evaluate(element => element.getBoundingClientRect().width)).toBeLessThan(expandedWidth);
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

  const sourceBeforeAppearance = await readFile(path.join(session.project, 'main.tex'), 'utf8');
  const pdfWidthBeforeAppearance = await secondPage.evaluate(element => element.getBoundingClientRect().width);
  const browserScale = await page.evaluate(() => window.visualViewport.scale);
  await page.getByTestId('settings-button').click();
  await page.getByTestId('editor-font-size').fill('16');
  await page.locator('#save-settings').click();
  await expect(page.getByTestId('editor-font-size')).not.toBeVisible();
  const editorSize = () => page.getByTestId('editor').locator('.cm-scroller').evaluate(element => parseFloat(getComputedStyle(element).fontSize));
  await expect.poll(editorSize).toBe(16);
  await expect(page.locator('#pdf-note')).not.toBeVisible();
  expect((await json(session, '/api/build')).sourceChanged).toBe(false);
  await editor.hover();
  await page.keyboard.down('Control');
  await page.mouse.wheel(0, -180);
  await page.keyboard.up('Control');
  await expect.poll(editorSize).toBeGreaterThan(16);
  const enlargedEditorSize = await editorSize();
  await page.keyboard.down('Control');
  await page.mouse.wheel(0, 90);
  await page.keyboard.up('Control');
  await expect.poll(editorSize).toBeLessThan(enlargedEditorSize);

  const beforePinch = await editorSize();
  const editorBounds = await page.getByTestId('editor').boundingBox();
  const centerX = Math.round(editorBounds.x + editorBounds.width / 2);
  const centerY = Math.round(editorBounds.y + editorBounds.height / 2);
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 2 });
  const points = distance => [{ x: centerX - distance, y: centerY, id: 1 }, { x: centerX + distance, y: centerY, id: 2 }];
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: points(30) });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: points(45) });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await expect.poll(editorSize).toBeGreaterThan(beforePinch);
  await cdp.send('Emulation.setTouchEmulationEnabled', { enabled: false });
  await cdp.detach();
  await expect(page.locator('#pdf-note')).not.toBeVisible();
  expect((await json(session, '/api/build')).sourceChanged).toBe(false);
  expect(await readFile(path.join(session.project, 'main.tex'), 'utf8')).toBe(sourceBeforeAppearance);
  expect(await secondPage.evaluate(element => element.getBoundingClientRect().width)).toBeCloseTo(pdfWidthBeforeAppearance, 0);
  expect(await page.evaluate(() => window.visualViewport.scale)).toBe(browserScale);
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  // Use the usual writing size for the remaining editing and screenshots.
  await page.getByTestId('settings-button').click();
  await page.getByTestId('editor-font-size').fill('14');
  await page.locator('#save-settings').click();
  await expect.poll(editorSize).toBe(14);

  let toolMissing = true;
  await page.route('**/api/state', async route => {
    const response = await route.fetch();
    const metadata = await response.json();
    if (toolMissing) {
      metadata.synctex = '';
      metadata.synctexError = 'synctex が見つかりません．';
      metadata.synctexInstallCommands = ['tlmgr install --reinstall synctex synctex.windows'];
    }
    await route.fulfill({ response, json: metadata });
  });
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect(page.getByTestId('synctex-forward')).toBeDisabled();
  toolMissing = false;
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect(page.getByTestId('synctex-forward')).toBeEnabled();
  await page.unroute('**/api/state');

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

test('PDF の倍率とページの回転に合わせて SyncTeX の位置を強調する', async ({ page }) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await openPDFViewer(page, pdfWithRotatedPages());

  for (const number of [1, 2, 3, 4]) {
    const location = { page: number, x: 40, y: 70, width: 80, height: 18 };
    await page.getByTestId('pdf-zoom').selectOption('0.75');
    expect(await page.evaluate(target => window.testViewer.showLocation(target), location)).toBe(true);
    await expect(page.getByTestId('pdf-page')).toHaveValue(String(number));

    for (const zoom of ['0.75', '1.5']) {
      if (zoom === '1.5') await page.getByTestId('pdf-zoom').selectOption(zoom);
      await expect(page.getByTestId('pdf-canvas')).toHaveAttribute('data-page', String(number));
      await expect(page.getByTestId('pdf-canvas')).toHaveAttribute('data-ready', 'true');
      await expect(page.getByTestId('synctex-highlight')).toBeVisible();
      const measured = await page.evaluate(pageNumber => {
        const view = window.testViewer.pageViewer.getPageView(pageNumber - 1);
        const canvas = view.canvas;
        const pixels = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data;
        let left = canvas.width, top = canvas.height, right = -1, bottom = -1;
        for (let y = 0; y < canvas.height; y++) {
          for (let x = 0; x < canvas.width; x++) {
            const offset = (y * canvas.width + x) * 4;
            if (pixels[offset + 3] > 200 && pixels[offset] < 32 && pixels[offset + 1] < 32 && pixels[offset + 2] < 32) {
              left = Math.min(left, x); top = Math.min(top, y);
              right = Math.max(right, x); bottom = Math.max(bottom, y);
            }
          }
        }
        const rect = canvas.getBoundingClientRect();
        const ink = {
          left: rect.left + left * rect.width / canvas.width,
          top: rect.top + top * rect.height / canvas.height,
          right: rect.left + (right + 1) * rect.width / canvas.width,
          bottom: rect.top + (bottom + 1) * rect.height / canvas.height,
        };
        const mark = view.div.querySelector('.synctex-highlight').getBoundingClientRect();
        return { ink, mark: { left: mark.left, top: mark.top, right: mark.right, bottom: mark.bottom },
          scale: view.viewport.scale, rotation: view.viewport.rotation, errors: window.viewerErrors };
      }, number);
      expect(measured.scale).toBeCloseTo(Number(zoom) * 4 / 3, 6);
      expect(measured.rotation).toBe((number - 1) * 90);
      expect(measured.ink.right).toBeGreaterThan(measured.ink.left);
      expect(measured.ink.bottom).toBeGreaterThan(measured.ink.top);
      // Compare the highlight with pixels rendered by PDF.js, including the
      // page rotation, zoom, and CSS rounding of the canvas.
      for (const [edge, padding] of [['left', 4], ['top', 3], ['right', -4], ['bottom', -3]]) {
        expect(Math.abs(measured.ink[edge] - measured.mark[edge] - padding), `${number}: ${zoom}, ${edge}`).toBeLessThan(2);
      }
      expect(measured.errors).toEqual([]);
      expect(errors).toEqual([]);

      const previousCount = await page.evaluate(() => window.inverseLocations.length);
      await page.mouse.dblclick((measured.ink.left + measured.ink.right) / 2, (measured.ink.top + measured.ink.bottom) / 2);
      await expect.poll(() => page.evaluate(() => window.inverseLocations.length)).toBe(previousCount + 1);
      const inverse = await page.evaluate(() => window.inverseLocations.at(-1));
      expect(inverse.buildId).toBe('test-build');
      expect(inverse.page).toBe(number);
      expect(Math.abs(inverse.x - (location.x + location.width / 2))).toBeLessThan(1);
      expect(Math.abs(inverse.y - (location.y + location.height / 2))).toBeLessThan(1);
    }
  }
  expect(await page.evaluate(() => window.viewerErrors)).toEqual([]);
  expect(errors).toEqual([]);
});

test('日本語のファイル名で原稿と PDF の位置を行き来する', async ({ page, session }, testInfo) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
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
  if (!forward.locations.length) await recordEmptySyncTeX(testInfo, session, finished.pdfBuildId, forward);
  expect(forward.locations.length, JSON.stringify(forward)).toBeGreaterThan(0);
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
  const response = await forwardResponse;
  expect(response.ok()).toBe(true);
  const requestedLocation = (await response.json()).locations.find(item => item.page === 2);
  expect(requestedLocation).toBeDefined();
  await expect(page.getByTestId('synctex-forward')).toBeEnabled();
  await expect(page.getByTestId('pdf-page')).toHaveValue('2');
  await expect(canvas).toHaveAttribute('data-page', '2');
  await expect(page.getByTestId('app-error')).not.toBeVisible();
  const highlight = page.getByTestId('synctex-highlight');
  await expect(highlight).toBeVisible();
  const box = await highlight.evaluate(element => ({
    left: parseFloat(element.style.left), top: parseFloat(element.style.top),
    width: parseFloat(element.style.width), height: parseFloat(element.style.height),
    page: element.closest('.page').dataset.pageNumber,
  }));
  expect(box.page).toBe('2');
  for (const value of [box.left, box.top, box.width, box.height]) expect(Number.isFinite(value)).toBe(true);
  expect(box.left).toBeGreaterThanOrEqual(-4);
  expect(box.top).toBeGreaterThanOrEqual(-3);
  expect(box.width).toBeGreaterThan(8);
  expect(box.height).toBeGreaterThan(6);
  expect(errors).toEqual([]);

  const clickedForward = page.waitForResponse(result =>
    new URL(result.url()).pathname === '/api/synctex/forward' && result.request().method() === 'POST');
  await editor.locator('.cm-line').filter({ hasText: 'これは SyncTeX' }).click({ modifiers: ['Control'] });
  expect((await clickedForward).ok()).toBe(true);
  await expect(page.getByTestId('synctex-forward')).toBeEnabled();
  await expect(page.getByTestId('app-error')).not.toBeVisible();
  await expect(highlight).toBeVisible();

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
  expect(errors).toEqual([]);
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
  const rendering = await page.locator('#pdf-stage').evaluate(viewer => {
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
  const renderingFile = testInfo.outputPath('pdf-rendering.json');
  await writeFile(renderingFile, JSON.stringify({ ...rendering, elapsedMS: finished.elapsedMs }, null, 2));
  await testInfo.attach('pdf-rendering.json', {
    path: renderingFile,
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
