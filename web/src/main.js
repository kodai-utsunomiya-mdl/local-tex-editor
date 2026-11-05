import { EditorState, Compartment, Prec } from '@codemirror/state';
import { EditorView, keymap } from '@codemirror/view';
import { basicSetup } from 'codemirror';
import { indentWithTab } from '@codemirror/commands';
import { StreamLanguage } from '@codemirror/language';
import { openSearchPanel } from '@codemirror/search';
import { autocompletion } from '@codemirror/autocomplete';
import { stex } from '@codemirror/legacy-modes/mode/stex';
import { PDFViewer } from './pdf-viewer.js';

const $ = id => document.getElementById(id);
const TOKEN_KEY = 'local-tex-editor-token';
const fragment = new URLSearchParams(location.hash.slice(1));
let token = fragment.get('token') || '';
try {
  if (token) sessionStorage.setItem(TOKEN_KEY, token);
  else token = sessionStorage.getItem(TOKEN_KEY) || '';
} catch { /* The current tab can work even when browser storage is disabled. */ }
if (fragment.has('token')) history.replaceState(null, '', location.pathname + location.search);

const state = {
  meta: null,
  documents: new Map(),
  pendingFiles: new Map(),
  treeButtons: new Map(),
  tabElements: new Map(),
  editor: null,
  activePath: '',
  projectGeneration: 0,
  openSequence: 0,
  operation: '',
  build: null,
  fullLog: '',
  pollTimer: null,
  polling: false,
  heartbeat: null,
  closed: false,
  connected: false,
  externalProjectChanged: false,
  reportedBuild: '',
};
const readOnly = new Compartment();
const modeLabels = { project: 'latexmkrc', pdflatex: 'pdfLaTeX', uplatex: 'upLaTeX', xelatex: 'XeLaTeX', lualatex: 'LuaLaTeX' };
let toastTimer = null;

class APIError extends Error {
  constructor(message, status = 0) { super(message); this.name = 'APIError'; this.status = status; }
}

function setConnected(connected) {
  state.connected = connected;
  const label = $('connection-status');
  label.classList.toggle('connected', connected);
  label.classList.toggle('disconnected', !connected);
  label.textContent = state.closed ? 'アプリを終了しました' : connected ? 'この PC に接続済み' : '接続を確認してください';
}

async function api(path, { method = 'GET', body, timeout = 15000, projectRoot = state.meta?.project?.root } = {}) {
  if (state.closed) throw new Error('アプリは終了しています．実行ファイルから開き直してください．');
  if (!token) throw new APIError('接続に必要な情報が見つかりません．アプリの実行ファイルを起動して開いたページを使用してください．', 401);
  const headers = { 'X-Local-Token': token, Accept: 'application/json' };
  if (projectRoot && !/^\/api\/(state|project\/|shutdown)/.test(path)) headers['X-Project-Root'] = encodeURIComponent(projectRoot);
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const controller = new AbortController();
  const timer = timeout ? setTimeout(() => controller.abort(), timeout) : null;
  try {
    const response = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store', signal: controller.signal });
    const text = await response.text();
    let data;
    try { data = text ? JSON.parse(text) : null; }
    catch { throw new APIError(`応答を読み取れませんでした (HTTP ${response.status})．${text.slice(0, 500)}`, response.status); }
    if (!response.ok) throw new APIError(data?.error || `処理に失敗しました (HTTP ${response.status})．`, response.status);
    setConnected(true);
    return data;
  } catch (error) {
    if (error instanceof APIError) {
      if (error.status === 401 || error.status === 403) setConnected(false);
      throw error;
    }
    setConnected(false);
    if (error.name === 'AbortError') throw new APIError('PC 上のアプリから応答がありません．処理が続いている場合は，少し待ってから再接続してください．');
    throw new APIError(`PC 上のアプリに接続できません．${error.message || error}`);
  } finally { if (timer) clearTimeout(timer); }
}

function showError(error, action = null) {
  const message = error?.message || String(error);
  $('error-text').textContent = message;
  $('app-error').hidden = false;
  const fallback = !state.connected && token && !state.closed ? { label: '再接続', run: reconnect } : null;
  const buttonAction = action || fallback;
  $('error-action').hidden = !buttonAction;
  $('error-action').textContent = buttonAction?.label || '再接続';
  $('error-action').onclick = buttonAction ? () => runTask(buttonAction.run) : null;
  if (error && typeof error === 'object') error.shown = true;
}

function clearError() { $('app-error').hidden = true; $('error-action').onclick = null; }
function toast(message) {
  clearTimeout(toastTimer);
  $('toast').textContent = message;
  $('toast').hidden = false;
  toastTimer = setTimeout(() => { $('toast').hidden = true; }, 3200);
}
function runTask(fn) { Promise.resolve().then(fn).catch(error => { if (!error?.shown) showError(error); }); }
function onClick(id, fn) { $(id).addEventListener('click', () => runTask(fn)); }
function normalise(text) { return (text || '').replace(/\r\n?/g, '\n'); }
function basename(path) { return path.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || path; }
function isDirty(doc) { return !!doc && !doc.state.doc.eq(doc.savedDoc); }
function dirtyDocuments() { return [...state.documents.values()].filter(isDirty); }
function activeDocument() { return state.documents.get(state.activePath); }
function isRunning() { return state.build?.status === 'running'; }

function setOperation(operation) {
  state.operation = operation;
  if (state.editor) state.editor.dispatch({ effects: readOnly.reconfigure([EditorState.readOnly.of(!!operation), EditorView.editable.of(!operation)]) });
  renderControls();
}

function updateCursor() {
  const view = state.editor;
  if (!view || !state.activePath) { $('cursor-status').textContent = ''; $('document-encoding').textContent = ''; return; }
  const head = view.state.selection.main.head;
  const line = view.state.doc.lineAt(head);
  $('cursor-status').textContent = `${line.number} 行，${head - line.from + 1} 列`;
  const doc = activeDocument();
  $('document-encoding').textContent = `UTF-8 · ${doc?.lineEnding === 'crlf' ? 'CRLF' : 'LF'}`;
}

const latexWords = [
  'begin', 'end', 'documentclass', 'usepackage', 'input', 'include', 'includegraphics', 'section', 'subsection', 'subsubsection',
  'paragraph', 'title', 'author', 'maketitle', 'label', 'ref', 'eqref', 'cref', 'Cref', 'cite', 'citep', 'citet', 'bibliography',
  'bibliographystyle', 'textbf', 'textit', 'emph', 'footnote', 'item', 'newcommand', 'renewcommand', 'frac', 'sqrt',
  'sum', 'prod', 'int', 'left', 'right', 'mathrm', 'mathbf', 'mathbb', 'mathcal', 'displaystyle', 'equation', 'align',
];
function latexCompletion(context) {
  const word = context.matchBefore(/\\[A-Za-z]*$/);
  if (!word) return null;
  return { from: word.from, options: latexWords.map(name => ({ label: `\\${name}`, type: 'keyword' })), validFor: /^\\[A-Za-z]*$/ };
}

const bibLanguage = StreamLanguage.define({
  startState: () => ({ quoted: false }),
  token(stream, parserState) {
    if (stream.eatSpace()) return null;
    if (parserState.quoted) {
      while (!stream.eol()) { const c = stream.next(); if (c === '\\') stream.next(); else if (c === '"') { parserState.quoted = false; break; } }
      return 'string';
    }
    if (stream.match(/^@[A-Za-z]+/)) return 'keyword';
    if (stream.match(/^%.*/)) return 'comment';
    if (stream.match(/^[A-Za-z][A-Za-z0-9_-]*(?=\s*=)/)) return 'propertyName';
    if (stream.match(/^\d+/)) return 'number';
    if (stream.eat('"')) { parserState.quoted = true; return 'string'; }
    if (stream.match(/^[{}(),=]/)) return 'punctuation';
    stream.next();
    return null;
  },
});

function makeEditorState(path, content) {
  const extension = path.split('.').pop().toLowerCase();
  const language = extension === 'bib' ? bibLanguage : /^(tex|sty|cls|ltx|def|bst)$/.test(extension) ? StreamLanguage.define(stex) : [];
  return EditorState.create({ doc: normalise(content), extensions: [
    basicSetup,
    language,
    EditorView.lineWrapping,
    readOnly.of([EditorState.readOnly.of(false), EditorView.editable.of(true)]),
    EditorView.contentAttributes.of({ 'aria-label': '原稿エディタ', spellcheck: 'false', autocapitalize: 'off', autocorrect: 'off' }),
    EditorState.phrases.of({ 'Find': '検索', 'Replace': '置換', 'next': '次', 'previous': '前', 'all': 'すべて', 'match case': '大文字と小文字を区別', 'by word': '単語単位', 'regexp': '正規表現', 'replace': '置換', 'replace all': 'すべて置換', 'close': '閉じる', 'Go to line': '行へ移動', 'go': '移動', 'Fold line': '折りたたむ', 'Unfold line': '展開' }),
    autocompletion({ override: /^(tex|sty|cls|ltx)$/.test(extension) ? [latexCompletion] : [] }),
    keymap.of([indentWithTab]),
    Prec.highest(keymap.of([
      { key: 'Mod-s', run: view => { if (view.composing) return false; runTask(saveCurrent); return true; } },
      { key: 'Mod-Shift-s', run: view => { if (view.composing) return false; runTask(saveEveryDocument); return true; } },
      { key: 'Mod-Enter', run: view => { if (view.composing) return false; runTask(startBuild); return true; } },
    ])),
    EditorView.updateListener.of(update => {
      const doc = activeDocument();
      if (doc) doc.state = update.state;
      if (update.docChanged && doc) { updateDocumentIndicators(doc); updatePreviewNote(); }
      if (update.selectionSet || update.docChanged) updateCursor();
    }),
  ] });
}

function makeDocument(file) {
  const editorState = makeEditorState(file.path, file.content);
  return { path: file.path, revision: file.revision, lineEnding: file.lineEnding || 'lf', state: editorState, savedDoc: editorState.doc, saving: null, conflict: null };
}

function selectDocument(doc) {
  state.activePath = doc.path;
  $('editor').hidden = false;
  $('editor-empty').hidden = true;
  if (state.editor) state.editor.setState(doc.state);
  else state.editor = new EditorView({ state: doc.state, parent: $('editor') });
  renderTabs();
  for (const [path, button] of state.treeButtons) { button.classList.toggle('active', path === doc.path); button.setAttribute('aria-current', path === doc.path ? 'true' : 'false'); }
  $('active-filename').textContent = doc.path;
  $('active-filename').title = doc.path;
  updateDocumentIndicators(doc);
  updateCursor();
  state.editor.focus();
  document.title = `${isDirty(doc) ? '● ' : ''}${basename(doc.path)} — Local TeX Editor`;
}

async function openFile(path) {
  if (state.operation || state.closed) return;
  const sequence = ++state.openSequence;
  const generation = state.projectGeneration;
  let doc = state.documents.get(path);
  if (!doc) {
    let promise = state.pendingFiles.get(path);
    if (!promise) {
      promise = api(`/api/file?path=${encodeURIComponent(path)}`).then(file => {
        if (generation !== state.projectGeneration) return null;
        const loaded = makeDocument(file);
        state.documents.set(path, loaded);
        return loaded;
      }).finally(() => { if (generation === state.projectGeneration) state.pendingFiles.delete(path); });
      state.pendingFiles.set(path, promise);
    }
    try { doc = await promise; }
    catch (error) { if (generation !== state.projectGeneration) return; throw error; }
  }
  if (doc && generation === state.projectGeneration && sequence === state.openSequence) selectDocument(doc);
}

function renderTabs() {
  const container = $('editor-tabs');
  const scroll = container.scrollLeft;
  container.replaceChildren();
  state.tabElements.clear();
  for (const doc of state.documents.values()) {
    const tab = document.createElement('div');
    tab.className = `editor-tab${state.activePath === doc.path ? ' active' : ''}`;
    const button = document.createElement('button');
    button.type = 'button'; button.className = 'tab-select'; button.setAttribute('role', 'tab');
    button.setAttribute('aria-selected', String(state.activePath === doc.path));
    button.title = doc.path; button.disabled = !!state.operation;
    const name = document.createElement('span'); name.textContent = basename(doc.path);
    const dirty = document.createElement('span'); dirty.className = 'tab-dirty'; dirty.hidden = !isDirty(doc); dirty.setAttribute('aria-label', '未保存');
    button.append(name, dirty);
    button.addEventListener('click', () => runTask(() => openFile(doc.path)));
    const close = document.createElement('button');
    close.type = 'button'; close.className = 'icon-button tab-close'; close.textContent = '×';
    close.setAttribute('aria-label', `${doc.path} を閉じる`); close.disabled = !!state.operation;
    close.addEventListener('click', () => runTask(() => closeDocument(doc)));
    tab.append(button, close); container.append(tab);
    state.tabElements.set(doc.path, { tab, button, dirty, close });
  }
  container.scrollLeft = scroll;
}

function renderTree() {
  const project = state.meta?.project;
  const filter = $('file-filter').value.trim().toLocaleLowerCase();
  const files = (project?.files || []).filter(file => file.path.toLocaleLowerCase().includes(filter));
  $('file-count').textContent = String(project?.files?.length || 0);
  $('file-tree').replaceChildren(); state.treeButtons.clear();
  if (!files.length) { const empty = document.createElement('p'); empty.className = 'tree-empty'; empty.textContent = filter ? '一致するファイルがありません．' : '編集できるファイルがありません．＋ からファイルを作成できます．'; $('file-tree').append(empty); return; }
  const tree = { directories: new Map(), files: [] };
  for (const file of files) {
    const parts = file.path.split('/'); let node = tree;
    for (const part of parts.slice(0, -1)) { if (!node.directories.has(part)) node.directories.set(part, { directories: new Map(), files: [] }); node = node.directories.get(part); }
    node.files.push(file);
  }
  function appendNode(node, parent, prefix = '') {
    const list = document.createElement('ul'); list.className = 'tree-list'; parent.append(list);
    for (const [name, child] of [...node.directories.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
      const item = document.createElement('li'); const details = document.createElement('details'); details.className = 'tree-directory';
      const path = prefix + name + '/'; details.open = !!filter || state.activePath.startsWith(path) || !prefix;
      const summary = document.createElement('summary'); summary.textContent = name; details.append(summary);
      appendNode(child, details, path); item.append(details); list.append(item);
    }
    for (const file of node.files.sort((a, b) => a.path.localeCompare(b.path))) {
      const item = document.createElement('li'); const button = document.createElement('button');
      button.type = 'button'; button.className = `file-button${file.path === state.activePath ? ' active' : ''}`;
      button.dataset.testid = `file-${file.path}`; button.dataset.path = file.path; button.title = file.path; button.disabled = !!state.operation;
      const type = document.createElement('span'); type.className = 'file-type'; type.setAttribute('aria-hidden', 'true');
      type.textContent = basename(file.path).includes('.') ? file.path.split('.').pop().slice(0, 4) : 'txt';
      const name = document.createElement('span'); name.className = 'file-name'; name.textContent = basename(file.path);
      const dirty = document.createElement('span'); dirty.className = 'file-unsaved'; dirty.hidden = !isDirty(state.documents.get(file.path));
      button.append(type, name, dirty); button.addEventListener('click', () => runTask(() => openFile(file.path)));
      item.append(button); list.append(item); state.treeButtons.set(file.path, button);
    }
  }
  appendNode(tree, $('file-tree'));
}

function updateDocumentIndicators(doc) {
  const dirty = isDirty(doc);
  const tab = state.tabElements.get(doc.path);
  if (tab) { tab.dirty.hidden = !dirty; tab.button.setAttribute('aria-label', `${doc.path}${dirty ? '，未保存' : ''}`); }
  const treeButton = state.treeButtons.get(doc.path);
  if (treeButton) treeButton.querySelector('.file-unsaved').hidden = !dirty;
  if (state.activePath === doc.path) {
    $('save-status').textContent = doc.saving ? '保存しています…' : doc.conflict ? '保存の確認が必要' : dirty ? '未保存' : '保存済み';
    $('save-status').classList.toggle('dirty', dirty && !doc.conflict);
    $('save-status').classList.toggle('error', !!doc.conflict);
    $('conflict-banner').hidden = !doc.conflict;
    document.title = `${dirty ? '● ' : ''}${basename(doc.path)} — Local TeX Editor`;
  }
  renderControls();
}

async function saveDocument(doc) {
  if (doc.saving) return doc.saving;
  if (!isDirty(doc)) return true;
  const snapshot = doc.state.doc;
  const generation = state.projectGeneration;
  const promise = (async () => {
    try {
      const result = await api('/api/file', { method: 'PUT', body: { path: doc.path, content: snapshot.toString(), revision: doc.revision } });
      if (generation !== state.projectGeneration) return false;
      doc.revision = result.revision;
      doc.lineEnding = result.lineEnding || doc.lineEnding;
      doc.savedDoc = snapshot;
      doc.conflict = null;
      if (state.build) state.build.sourceChanged = true;
      updatePreviewNote();
      return true;
    } catch (error) {
      if (error.status === 409) doc.conflict = error;
      const wrapped = new APIError(`${doc.path} を保存できませんでした．${error.message || error}`, error.status);
      showError(wrapped, error.status === 409 ? { label: '内容を確認', run: () => resolveConflict(doc) } : null);
      throw wrapped;
    } finally {
      doc.saving = null;
      if (generation === state.projectGeneration) { updateDocumentIndicators(doc); updateCursor(); }
    }
  })();
  doc.saving = promise;
  updateDocumentIndicators(doc);
  return promise;
}

async function saveAll() {
  for (const doc of state.documents.values()) {
    if (doc.saving) await doc.saving;
    if (isDirty(doc)) await saveDocument(doc);
  }
}

async function saveCurrent() {
  if (state.operation || !activeDocument()) return;
  const doc = activeDocument();
  if (doc.saving) await doc.saving;
  if (!isDirty(doc)) { toast('保存済みです．'); return; }
  await saveDocument(doc);
  clearError();
  toast(isDirty(doc) ? '保存しました．その後の編集は未保存です．' : '保存しました．');
}

async function saveEveryDocument() {
  if (state.operation || !state.meta?.project) return;
  setOperation('saving');
  try { await saveAll(); clearError(); toast('開いているファイルを保存しました．'); }
  finally { setOperation(''); }
}

function choose({ title, description, primary = '保存して続ける', secondary = '保存せずに続ける', cancel = 'キャンセル' }) {
  const dialog = $('decision-dialog');
  $('decision-title').textContent = title; $('decision-description').textContent = description;
  $('decision-primary').textContent = primary; $('decision-secondary').textContent = secondary; $('decision-secondary').hidden = !secondary;
  $('decision-cancel').textContent = cancel;
  return new Promise(resolve => {
    let settled = false;
    const finish = value => { if (settled) return; settled = true; dialog.removeEventListener('cancel', cancelHandler); dialog.removeEventListener('close', closeHandler); if (dialog.open) dialog.close(); resolve(value); };
    const cancelHandler = event => { event.preventDefault(); finish('cancel'); };
    const closeHandler = () => finish('cancel');
    $('decision-primary').onclick = () => finish('primary');
    $('decision-secondary').onclick = () => finish('secondary');
    $('decision-cancel').onclick = () => finish('cancel');
    dialog.addEventListener('cancel', cancelHandler); dialog.addEventListener('close', closeHandler);
    dialog.showModal();
  });
}

async function confirmUnsaved(description) {
  const pending = [...state.documents.values()].map(doc => doc.saving).filter(Boolean);
  await Promise.allSettled(pending);
  const dirty = dirtyDocuments();
  if (!dirty.length) return true;
  const answer = await choose({ title: '未保存の変更があります', description: `${dirty.map(doc => doc.path).join('\n')}\n\n${description}` });
  if (answer === 'cancel') return false;
  if (answer === 'primary') await saveAll();
  return true;
}

async function closeDocument(doc) {
  if (state.operation) return;
  if (doc.saving) await doc.saving;
  if (isDirty(doc)) {
    const answer = await choose({ title: '未保存のファイルを閉じますか？', description: doc.path, primary: '保存して閉じる', secondary: '保存せずに閉じる' });
    if (answer === 'cancel') return;
    if (answer === 'primary') { await saveDocument(doc); if (isDirty(doc)) return; }
  }
  const paths = [...state.documents.keys()]; const index = paths.indexOf(doc.path);
  state.documents.delete(doc.path);
  if (state.activePath === doc.path) {
    const next = paths[index + 1] || paths[index - 1];
    if (next && state.documents.has(next)) selectDocument(state.documents.get(next));
    else {
      state.activePath = ''; state.editor?.destroy(); state.editor = null; $('editor').replaceChildren();
      $('editor').hidden = true; $('editor-empty').hidden = false; $('active-filename').textContent = '原稿';
      $('save-status').textContent = ''; $('conflict-banner').hidden = true; updateCursor(); document.title = 'Local TeX Editor';
    }
  }
  renderTabs(); renderTree(); renderControls(); updatePreviewNote();
}

async function resolveConflict(doc) {
  if (!doc || state.operation) return;
  const remote = await api(`/api/file?path=${encodeURIComponent(doc.path)}`);
  $('conflict-description').textContent = `${doc.path} は，読み込んだ後にディスクの内容が変更されました．保存する内容を選んでください．`;
  $('disk-content').value = remote.content;
  $('editing-content').value = doc.state.doc.toString();
  const dialog = $('conflict-dialog');
  const choice = await new Promise(resolve => {
    let done = false;
    const finish = value => { if (done) return; done = true; dialog.removeEventListener('close', close); if (dialog.open) dialog.close(); resolve(value); };
    const close = () => finish('cancel');
    $('conflict-cancel').onclick = () => finish('cancel');
    $('conflict-use-disk').onclick = () => finish('disk');
    $('conflict-overwrite').onclick = () => finish('overwrite');
    dialog.addEventListener('close', close); dialog.showModal();
  });
  if (choice === 'cancel') return;
  if (choice === 'disk') {
    const content = normalise(remote.content);
    doc.revision = remote.revision; doc.lineEnding = remote.lineEnding || 'lf'; doc.conflict = null;
    const transaction = doc.state.update({ changes: { from: 0, to: doc.state.doc.length, insert: content } });
    doc.savedDoc = transaction.state.doc;
    if (state.activePath === doc.path) state.editor.update([transaction]); else doc.state = transaction.state;
    clearError(); updateDocumentIndicators(doc); updatePreviewNote(); toast('ディスクの内容を読み込みました．');
  } else {
    doc.revision = remote.revision;
    doc.savedDoc = EditorState.create({ doc: normalise(remote.content) }).doc;
    await saveDocument(doc);
    doc.conflict = null;
    clearError(); updateDocumentIndicators(doc); toast('編集中の内容を保存しました．');
  }
}

function recentPaths() {
  return (state.meta?.recentProjects || []).map(item => typeof item === 'string' ? item : item.root || item.path || '').filter(Boolean).slice(0, 6);
}
function renderRecent() {
  const paths = recentPaths();
  for (const id of ['welcome-recent', 'project-recent']) $(id).hidden = paths.length === 0;
  for (const id of ['welcome-recent-list', 'project-recent-list']) {
    $(id).replaceChildren();
    for (const path of paths) {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'recent-project'; button.title = path;
      const name = document.createElement('strong'); name.textContent = basename(path);
      const detail = document.createElement('span'); detail.textContent = path; button.append(name, detail);
      button.addEventListener('click', () => { showProjectDialog(); $('project-path').value = path; });
      $(id).append(button);
    }
  }
}

async function applyMeta(meta, resetProject = false) {
  if (!resetProject && state.meta?.project && meta.project?.root !== state.meta.project.root) {
    handleExternalProjectChange();
    return;
  }
  const generation = resetProject ? ++state.projectGeneration : state.projectGeneration;
  state.meta = meta;
  state.externalProjectChanged = false;
  if (resetProject) {
    clearTimeout(state.pollTimer); state.pollTimer = null;
    state.documents.clear(); state.pendingFiles.clear(); state.activePath = ''; ++state.openSequence;
    state.editor?.destroy(); state.editor = null; $('editor').replaceChildren(); $('editor').hidden = true; $('editor-empty').hidden = false;
    $('conflict-banner').hidden = true; $('file-filter').value = ''; $('save-status').textContent = ''; $('active-filename').textContent = '原稿';
    state.build = null; state.reportedBuild = ''; await viewer.reset();
  }
  if (generation !== state.projectGeneration) return;
  const project = meta.project;
  $('welcome').hidden = !!project; $('workbench').hidden = !project;
  $('project-name').textContent = project ? basename(project.root) : 'プロジェクトを開く';
  $('project-name').title = project?.root || '';
  $('project-root').textContent = project?.root || ''; $('project-root').title = project?.root || '';
  $('app-version').textContent = meta.version ? `v${meta.version}` : '';
  $('main-document-label').textContent = project ? `${project.main || 'メイン文書を選択'} · ${modeLabels[project.mode] || project.mode}` : 'メイン文書を選択';
  renderRecent(); renderTree(); renderTabs(); renderControls(); updateCursor();
  applyBuild(meta.build);
  if (project && !state.activePath && !state.operation) {
    const file = project.files?.find(file => file.path === project.main) || project.files?.find(file => /\.tex$/i.test(file.path)) || project.files?.[0];
    if (file) await openFile(file.path);
  }
}

function showProjectDialog() {
  if (state.operation || state.closed) return;
  if (isRunning()) { showError(new Error('コンパイルが終了してからプロジェクトを切り替えてください．')); return; }
  $('project-path').value = state.meta?.project?.root || '';
  $('project-tex-bin').value = state.meta?.texBin || '';
  $('project-tex-help').textContent = state.meta?.latexmk ? `latexmk: ${state.meta.latexmk}` : '例: C:\\texlive\\2025\\bin\\windows';
  $('project-error').hidden = true; renderRecent();
  if (!$('project-dialog').open) $('project-dialog').showModal();
  $('project-path').focus();
}

function setDialogBusy(dialog, busy) {
  dialog.dataset.busy = String(busy);
  for (const element of dialog.querySelectorAll('button, input, select')) element.disabled = busy;
}

async function openProject(event) {
  event.preventDefault();
  if (state.operation) return;
  const path = $('project-path').value.trim(); const texBin = $('project-tex-bin').value.trim();
  if (!path) return;
  const dialog = $('project-dialog'); $('project-error').hidden = true;
  setOperation('opening'); setDialogBusy(dialog, true);
  try {
    if (!await confirmUnsaved('プロジェクトを切り替える前に，変更を保存しますか？')) return;
    if (texBin !== (state.meta?.texBin || '')) await api('/api/settings', { method: 'PUT', body: { texBin } });
    const meta = await api('/api/project/open', { method: 'POST', body: { path } });
    await applyMeta(meta, true);
    dialog.close(); clearError();
  } catch (error) { $('project-error').textContent = error.message || String(error); $('project-error').hidden = false; }
  finally {
    setDialogBusy(dialog, false); setOperation('');
    if (state.meta?.project && !state.activePath) {
      const file = state.meta.project.files?.find(file => file.path === state.meta.project.main) || state.meta.project.files?.[0];
      if (file) runTask(() => openFile(file.path));
    }
  }
}

async function pickProject() {
  $('pick-project').disabled = true;
  try {
    const result = await api('/api/project/pick', { method: 'POST', body: {}, timeout: 0 });
    if (result.path) $('project-path').value = result.path;
  } catch (error) { $('project-error').textContent = error.message || String(error); $('project-error').hidden = false; }
  finally { $('pick-project').disabled = false; }
}

function showSettings() {
  if (state.operation || state.closed) return;
  if (isRunning()) { showError(new Error('コンパイルが終了してから設定を変更してください．')); return; }
  const project = state.meta?.project;
  $('tex-bin').value = state.meta?.texBin || '';
  $('tex-tool-status').textContent = state.meta?.latexmk ? `latexmk: ${state.meta.latexmk}` : state.meta?.toolError || '例: C:\\texlive\\2025\\bin\\windows．空欄の場合は自動で検索します．';
  $('main-file').replaceChildren();
  for (const file of project?.files || []) if (/\.tex$/i.test(file.path)) { const option = document.createElement('option'); option.value = file.path; option.textContent = file.path; $('main-file').append(option); }
  if (project?.main && ![...$('main-file').options].some(option => option.value === project.main)) { const option = document.createElement('option'); option.value = project.main; option.textContent = project.main; $('main-file').append(option); }
  $('main-file').value = project?.main || '';
  $('compile-mode').value = project?.mode || 'pdflatex';
  $('main-field').hidden = !project; $('mode-field').hidden = !project;
  $('settings-error').hidden = true; $('settings-dialog').showModal();
}

async function saveSettings(event) {
  event.preventDefault(); if (state.operation) return;
  const body = { texBin: $('tex-bin').value.trim() };
  if (state.meta?.project) { body.main = $('main-file').value; body.mode = $('compile-mode').value; }
  setOperation('settings'); setDialogBusy($('settings-dialog'), true); $('settings-error').hidden = true;
  try { const meta = await api('/api/settings', { method: 'PUT', body }); await applyMeta(meta); $('settings-dialog').close(); clearError(); toast('設定を保存しました．'); }
  catch (error) { $('settings-error').textContent = error.message || String(error); $('settings-error').hidden = false; }
  finally { setDialogBusy($('settings-dialog'), false); setOperation(''); }
}

async function refreshFiles() {
  if (state.operation || !state.meta?.project) return;
  const meta = await api('/api/state');
  if (meta.project?.root !== state.meta.project.root) { handleExternalProjectChange(); return; }
  await applyMeta(meta); toast('ファイル一覧を更新しました．');
}

async function createFile(event) {
  event.preventDefault(); if (state.operation) return;
  const path = $('new-file-path').value.trim().replace(/\\/g, '/'); if (!path) return;
  setOperation('creating'); setDialogBusy($('new-file-dialog'), true); $('new-file-error').hidden = true;
  let created = null;
  try {
    const file = await api('/api/file', { method: 'PUT', body: { path, content: '', revision: '' } });
    created = makeDocument(file); state.documents.set(file.path, created);
    const meta = await api('/api/state'); await applyMeta(meta);
    $('new-file-dialog').close(); clearError();
  } catch (error) { $('new-file-error').textContent = error.message || String(error); $('new-file-error').hidden = false; }
  finally { setDialogBusy($('new-file-dialog'), false); setOperation(''); }
  if (created) selectDocument(created);
}

function setLogVisible(visible) { $('log-panel').hidden = !visible; $('log-toggle').setAttribute('aria-expanded', String(visible)); }
function formatElapsed(ms) { const seconds = Math.max(0, Math.round(ms / 1000)); return seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`; }
function updateElapsed() {
  const build = state.build;
  const ms = build?.status === 'running' && build.startedAt ? Date.now() - new Date(build.startedAt).getTime() : build?.elapsedMs;
  $('build-elapsed').textContent = typeof ms === 'number' && Number.isFinite(ms) ? formatElapsed(ms) : '';
}

function updatePreviewNote() {
  const build = state.build;
  const note = $('pdf-note'); const label = $('pdf-state');
  note.hidden = true; note.classList.remove('error'); label.classList.remove('stale');
  if (!build?.pdfReady) { label.textContent = ''; return; }
  if (build.status === 'failed') { note.textContent = '最新のコンパイルに失敗しました．前回成功した PDF を表示しています．'; note.classList.add('error'); label.textContent = '前回の PDF'; }
  else if (build.status === 'cancelled') { note.textContent = 'コンパイルを停止しました．前回成功した PDF を表示しています．'; label.textContent = '前回の PDF'; }
  else if (build.status === 'running') { note.textContent = 'コンパイル中です．完了すると PDF が更新されます．'; label.textContent = '更新待ち'; }
  else if (build.sourceChanged || dirtyDocuments().length) { note.textContent = '原稿に変更があります．最新の内容を確認するにはコンパイルしてください．'; label.textContent = '原稿に変更あり'; }
  else { label.textContent = viewer.document && viewer.buildId === build.pdfBuildId ? '更新済み' : '読み込み中'; return; }
  note.hidden = false; label.classList.add('stale');
}

function applyBuild(build) {
  if (build && state.build) {
    if (build.id === state.build.id && state.build.status !== 'running' && build.status === 'running') return;
    if (build.id !== state.build.id && Date.parse(build.startedAt) < Date.parse(state.build.startedAt)) return;
  }
  state.build = build || null;
  const labels = { running: 'コンパイル中', success: 'コンパイル完了', failed: 'コンパイル失敗', cancelled: '停止しました' };
  $('build-status').textContent = labels[build?.status] || 'コンパイル前';
  $('build-status').dataset.status = build?.status || 'idle';
  $('log-indicator').hidden = build?.status !== 'failed';
  updateElapsed();
  if (build) {
    const text = [build.command ? `$ ${build.command}` : '', build.log || '', build.error ? `\n${build.error}` : ''].filter(Boolean).join('\n').replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '');
    state.fullLog = text || 'プロセスからの出力を待っています…';
    const output = $('build-log'); const follow = output.scrollHeight - output.scrollTop - output.clientHeight < 55;
    const displayed = state.fullLog.length > 300000 ? `ログの末尾 300,000 文字を表示しています．「コピー」で受け取ったログ全体を取得できます．\n\n${state.fullLog.slice(-300000)}` : state.fullLog;
    if (output.textContent !== displayed) { output.textContent = displayed; if (follow) output.scrollTop = output.scrollHeight; }
    const exit = Number.isInteger(build.exitCode) ? `終了コード ${build.exitCode}` : '実行中';
    $('log-meta').textContent = `${exit}${build.finishedAt ? ` · ${formatElapsed(build.elapsedMs || 0)}` : ''}`;
    if (build.status === 'failed' && state.reportedBuild !== `${build.id}:failed`) {
      state.reportedBuild = `${build.id}:failed`; setLogVisible(true);
      showError(new Error(build.error || `コンパイルに失敗しました (終了コード ${build.exitCode ?? '不明'})．ログを確認してください．`));
    }
    if (build.pdfReady && build.pdfBuildId && state.meta?.project) viewer.load(build.pdfBuildId, state.meta.project.root).catch(error => showError(error));
    if (build.status === 'running') schedulePoll(650);
    else { clearTimeout(state.pollTimer); state.pollTimer = null; }
  } else {
    state.fullLog = ''; $('build-log').textContent = 'コンパイルすると，ここにコマンドとログが表示されます．'; $('log-meta').textContent = '';
    if (viewer.document || viewer.loadingTask) viewer.reset().catch(error => showError(error));
  }
  updatePreviewNote(); renderControls();
}

function schedulePoll(delay) {
  clearTimeout(state.pollTimer);
  if (state.closed) return;
  state.pollTimer = setTimeout(() => runTask(pollBuild), delay);
}
async function pollBuild() {
  if (state.polling || state.closed || !state.meta?.project) return;
  state.polling = true;
  const generation = state.projectGeneration;
  const expectedId = state.build?.id;
  try {
    const build = await api('/api/build');
    if (generation !== state.projectGeneration) return;
    if (expectedId && state.build?.id !== expectedId && build?.id !== state.build?.id) return;
    applyBuild(build);
  }
  catch (error) { if (generation === state.projectGeneration) { showError(error); if (isRunning()) schedulePoll(3000); } }
  finally { state.polling = false; }
}

async function startBuild() {
  if (state.operation || isRunning() || !state.meta?.project || state.closed) return;
  if (state.editor?.composing) return;
  if (!state.meta.project.main) { showSettings(); return; }
  setOperation('compiling'); $('build-status').textContent = 'ファイルを保存しています…'; clearError();
  try {
    await saveAll();
    const build = await api('/api/build', { method: 'POST', body: {} });
    setLogVisible(true); applyBuild(build);
  } catch (error) { if (!error?.shown) showError(error); }
  finally { setOperation(''); if (!isRunning()) { const current = state.build; $('build-status').textContent = current ? ({ success: 'コンパイル完了', failed: 'コンパイル失敗', cancelled: '停止しました' }[current.status] || 'コンパイル前') : 'コンパイル前'; } }
}

async function stopBuild() {
  if (!isRunning() || state.operation) return;
  $('stop-button').disabled = true;
  try { const build = await api('/api/build/stop', { method: 'POST', body: {}, timeout: 30000 }); applyBuild(build); }
  finally { renderControls(); }
}

function renderControls() {
  const project = state.meta?.project;
  const busy = !!state.operation || state.closed;
  const doc = activeDocument();
  const running = isRunning();
  $('compile-button').disabled = busy || running || !project || !project.main || state.externalProjectChanged;
  $('compile-label').textContent = state.operation === 'compiling' ? '保存しています…' : running ? 'コンパイル中' : 'コンパイル';
  $('compile-icon').classList.toggle('spinning', running || state.operation === 'compiling');
  $('compile-icon').textContent = running || state.operation === 'compiling' ? '' : '▶';
  $('stop-button').hidden = !running; $('stop-button').disabled = busy || !running;
  $('save-button').disabled = busy || !doc || !!doc.saving || !isDirty(doc) || state.externalProjectChanged;
  $('find-button').disabled = !doc || state.closed;
  $('main-document').disabled = busy || !project || running;
  $('settings-button').disabled = busy || running;
  $('show-project').disabled = busy || running;
  $('welcome-open').disabled = busy;
  $('shutdown-button').disabled = busy;
  $('new-file').disabled = busy || !project;
  $('refresh-files').disabled = busy || !project;
  $('download-pdf').disabled = !state.build?.pdfReady || state.closed;
  $('copy-log').disabled = !state.fullLog;
  for (const button of state.treeButtons.values()) button.disabled = busy;
  for (const tab of state.tabElements.values()) { tab.button.disabled = busy; tab.close.disabled = busy; }
}

async function downloadPDF() {
  const build = state.build; if (!build?.pdfReady) return;
  const project = state.meta.project;
  const response = await fetch(`/api/pdf?build=${encodeURIComponent(build.pdfBuildId)}`, { headers: { 'X-Local-Token': token, 'X-Project-Root': encodeURIComponent(project.root) }, cache: 'no-store' });
  if (!response.ok) { const text = await response.text(); let message = text; try { message = JSON.parse(text).error || text; } catch { /* A non-JSON error is still displayed. */ } throw new APIError(`PDF を取得できませんでした．${message}`, response.status); }
  const blob = await response.blob(); const url = URL.createObjectURL(blob);
  const link = document.createElement('a'); link.href = url; link.download = basename(project.main).replace(/\.tex$/i, '') + '.pdf'; link.click();
  setTimeout(() => URL.revokeObjectURL(url), 60000);
}

async function copyLog() {
  if (!state.fullLog) return;
  await navigator.clipboard.writeText(state.fullLog);
  toast('ログをコピーしました．');
}

function handleExternalProjectChange() {
  state.externalProjectChanged = true; renderControls();
  showError(new Error('別のタブでプロジェクトが切り替わりました．この画面の編集内容は残っています．作業中のフォルダーを開き直してください．'), {
    label: '元のフォルダーへ戻る',
    run: async () => { const meta = await api('/api/project/open', { method: 'POST', body: { path: state.meta.project.root } }); await applyMeta(meta); clearError(); },
  });
}

async function reconnect() {
  const meta = await api('/api/state');
  if (state.meta?.project && meta.project?.root !== state.meta.project.root) { handleExternalProjectChange(); return; }
  await applyMeta(meta, !state.meta); clearError();
}

async function heartbeat() {
  if (state.closed || state.operation) return;
  const generation = state.projectGeneration;
  try {
    const meta = await api('/api/state');
    if (generation !== state.projectGeneration) return;
    if (state.meta?.project && meta.project?.root !== state.meta.project.root) { if (!state.externalProjectChanged) handleExternalProjectChange(); return; }
    if (!isRunning() && meta.build && meta.build.id !== state.build?.id) applyBuild(meta.build);
  } catch (error) { if ($('app-error').hidden) showError(error); }
}

async function shutdown() {
  if (state.operation || state.closed) return;
  setOperation('shutdown');
  try {
    if (!await confirmUnsaved('アプリを終了する前に，変更を保存しますか？')) return;
    if (isRunning()) {
      const answer = await choose({ title: 'コンパイルを停止して終了しますか？', description: '実行中の TeX とローカルのアプリを終了します．', primary: '停止して終了', secondary: '' });
      if (answer !== 'primary') return;
    }
    await api('/api/shutdown', { method: 'POST', body: {}, timeout: 30000 });
    state.closed = true; clearTimeout(state.pollTimer); clearInterval(state.heartbeat); setConnected(false);
    try { sessionStorage.removeItem(TOKEN_KEY); } catch { /* No persistent storage is required. */ }
    $('workbench').hidden = true; $('welcome').hidden = false;
    $('welcome').querySelector('h1').textContent = 'アプリを終了しました';
    $('welcome').querySelector('.welcome-description').textContent = 'このタブを閉じてください．次回は実行ファイルから開けます．';
    $('welcome-open').hidden = true; $('welcome-recent').hidden = true; $('welcome').querySelector('.welcome-note').hidden = true;
    clearError();
  } finally { setOperation(state.closed ? 'closed' : ''); }
}

const viewer = new PDFViewer({
  token,
  onError: error => showError(error, { label: 'PDF を再表示', run: async () => { clearError(); await viewer.retry(); } }),
  onReady: () => updatePreviewNote(),
});

onClick('show-project', showProjectDialog); onClick('welcome-open', showProjectDialog);
onClick('settings-button', showSettings); onClick('main-document', showSettings);
onClick('pick-project', pickProject); onClick('shutdown-button', shutdown);
onClick('save-button', saveCurrent); onClick('compile-button', startBuild); onClick('stop-button', stopBuild);
onClick('refresh-files', refreshFiles); onClick('download-pdf', downloadPDF); onClick('copy-log', copyLog);
onClick('find-button', () => { if (state.editor) { openSearchPanel(state.editor); state.editor.focus(); } });
onClick('dismiss-error', clearError); onClick('review-conflict', () => resolveConflict(activeDocument()));
onClick('log-toggle', () => setLogVisible($('log-panel').hidden)); onClick('close-log', () => setLogVisible(false));
onClick('new-file', () => { if (!state.meta?.project) return; $('new-file-path').value = ''; $('new-file-error').hidden = true; $('new-file-dialog').showModal(); });
$('project-form').addEventListener('submit', event => { runTask(() => openProject(event)); event.preventDefault(); });
$('settings-form').addEventListener('submit', event => { runTask(() => saveSettings(event)); event.preventDefault(); });
$('new-file-form').addEventListener('submit', event => { runTask(() => createFile(event)); event.preventDefault(); });
$('file-filter').addEventListener('input', renderTree);
for (const button of document.querySelectorAll('[data-close-dialog]')) button.addEventListener('click', () => {
  const dialog = $(button.dataset.closeDialog); if (dialog.dataset.busy !== 'true') dialog.close();
});
for (const dialog of document.querySelectorAll('dialog')) dialog.addEventListener('cancel', event => { if (dialog.dataset.busy === 'true') event.preventDefault(); });

onClick('sidebar-toggle', () => {
  const collapsed = $('app-shell').classList.toggle('sidebar-collapsed');
  $('sidebar-toggle').setAttribute('aria-expanded', String(!collapsed));
});
if (matchMedia('(max-width: 640px)').matches) { $('app-shell').classList.add('sidebar-collapsed'); $('sidebar-toggle').setAttribute('aria-expanded', 'false'); }
function switchView(preview) {
  $('app-shell').classList.toggle('preview-mode', preview);
  $('show-preview').classList.toggle('selected', preview); $('show-editor').classList.toggle('selected', !preview);
  $('show-preview').setAttribute('aria-pressed', String(preview)); $('show-editor').setAttribute('aria-pressed', String(!preview));
}
onClick('show-editor', () => switchView(false)); onClick('show-preview', () => switchView(true));

function setSplit(clientX) {
  const editor = $('editor-pane').getBoundingClientRect();
  const pdf = $('pdf-pane').getBoundingClientRect();
  const total = pdf.right - editor.left - 5;
  if (total <= 520) return;
  const left = Math.min(total - 260, Math.max(260, clientX - editor.left));
  $('workbench').style.setProperty('--editor-size', `${left / total}fr`);
  $('workbench').style.setProperty('--preview-size', `${1 - left / total}fr`);
}
$('splitter').addEventListener('pointerdown', event => {
  if (event.button !== 0) return;
  event.preventDefault(); const splitter = $('splitter'); splitter.setPointerCapture(event.pointerId);
  document.body.classList.add('resizing'); splitter.classList.add('dragging');
  const move = moveEvent => setSplit(moveEvent.clientX);
  const finish = () => { document.body.classList.remove('resizing'); splitter.classList.remove('dragging'); splitter.removeEventListener('pointermove', move); splitter.removeEventListener('pointerup', finish); splitter.removeEventListener('pointercancel', finish); };
  splitter.addEventListener('pointermove', move); splitter.addEventListener('pointerup', finish); splitter.addEventListener('pointercancel', finish);
});
$('splitter').addEventListener('keydown', event => {
  if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') { event.preventDefault(); setSplit($('splitter').getBoundingClientRect().left + (event.key === 'ArrowLeft' ? -25 : 25)); }
});

document.addEventListener('keydown', event => {
  if (!event.ctrlKey && !event.metaKey) return;
  if (event.isComposing || state.editor?.composing || document.querySelector('dialog[open]')) return;
  if (event.target.closest('.cm-editor')) return;
  if (event.key.toLowerCase() === 's') { event.preventDefault(); runTask(event.shiftKey ? saveEveryDocument : saveCurrent); }
  else if (event.key === 'Enter') { event.preventDefault(); runTask(startBuild); }
});
window.addEventListener('beforeunload', event => { if (!state.closed && (dirtyDocuments().length || [...state.documents.values()].some(doc => doc.saving))) { event.preventDefault(); event.returnValue = ''; } });
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') runTask(heartbeat); });
setInterval(() => { if (isRunning()) updateElapsed(); }, 1000);

async function initialise() {
  try {
    const meta = await api('/api/state');
    await applyMeta(meta, true);
    state.heartbeat = setInterval(() => runTask(heartbeat), 30000);
  } catch (error) { showError(error); }
}
initialise();
