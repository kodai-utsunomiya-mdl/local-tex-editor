import test from 'node:test';
import assert from 'node:assert/strict';
import { EditorState } from '@codemirror/state';
import { setSearchQuery, replaceAll } from '@codemirror/search';
import { sourceQuery, sourceSearch } from '../web/src/source-search.js';

const block = String.raw`\ifneuripslayout
  \input{common-neurips-uplatex}
\else
  \input{common-uplatex}
\fi`;

test('search finds a pasted TeX block with Windows line endings', () => {
  const doc = `before\n${block}\nafter\n${block}\n`;
  const query = sourceQuery(block.replaceAll('\n', '\r\n'));
  const cursor = query.getCursor(EditorState.create({ doc }));
  const matches = [];
  while (!cursor.next().done) matches.push(doc.slice(cursor.value.from, cursor.value.to));
  assert.deepEqual(matches, [block, block]);
});

test('TeX backslashes remain literal when searching and replacing multiple lines', () => {
  const original = String.raw`\newcommand{\test}{old}
\textbf{first} \\ next`;
  const replacement = String.raw`\newcommand{\test}{new}
\textbf{second} \\ next`;
  let state = EditorState.create({ doc: `${original}\n\n${original}`, extensions: sourceSearch() });
  state = state.update({ effects: setSearchQuery.of(sourceQuery(original, { replace: replacement })) }).state;
  const view = { get state() { return state; }, dispatch(spec) { state = state.update(spec).state; } };
  assert.equal(replaceAll(view), true);
  assert.equal(state.doc.toString(), `${replacement}\n\n${replacement}`);
});

test('search accepts explicit regular expressions and reports invalid expressions', () => {
  const query = sourceQuery(String.raw`\\(?:input|include)\{([^}]+)\}`, { regexp: true });
  const cursor = query.getCursor(EditorState.create({ doc: String.raw`\input{chapter} and \include{appendix}` }));
  assert.equal(query.valid, true);
  assert.equal(cursor.next().value.match[1], 'chapter');
  assert.equal(cursor.next().value.match[1], 'appendix');
  assert.equal(sourceQuery('[', { regexp: true }).valid, false);
});
