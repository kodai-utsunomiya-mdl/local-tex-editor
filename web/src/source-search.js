import { Prec } from '@codemirror/state';
import { keymap, runScopeHandlers } from '@codemirror/view';
import {
  search, SearchQuery, getSearchQuery, setSearchQuery, openSearchPanel,
  closeSearchPanel, findNext, findPrevious, replaceNext, replaceAll,
} from '@codemirror/search';

const normalise = text => String(text || '').replace(/\r\n?/g, '\n');

export function sourceQuery(searchText, options = {}) {
  return new SearchQuery({
    ...options,
    search: normalise(searchText),
    replace: normalise(options.replace),
    literal: true,
  });
}

export function openSourceSearchPanel(view) {
  const selection = view.state.selection.main;
  const selected = !selection.empty && selection.to - selection.from <= 100_000
    ? view.state.sliceDoc(selection.from, selection.to) : null;
  openSearchPanel(view);
  if (selected !== null) {
    const current = getSearchQuery(view.state);
    view.dispatch({ effects: setSearchQuery.of(sourceQuery(selected, {
      caseSensitive: current.caseSensitive, wholeWord: current.wholeWord,
      replace: current.replace, regexp: false,
    })) });
  }
  return true;
}

export function sourceSearch() {
  return [
    search({ top: true, literal: true, createPanel: view => new SourceSearchPanel(view) }),
    Prec.high(keymap.of([{ key: 'Mod-f', run: openSourceSearchPanel, scope: 'editor search-panel' }])),
  ];
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

class SourceSearchPanel {
  constructor(view) {
    this.view = view;
    this.query = getSearchQuery(view.state);
    this.timer = null;
    this.matches = [];
    this.truncated = false;
    this.dom = element('div', 'cm-search source-search');
    this.dom.setAttribute('aria-label', '原稿の検索と置換');
    this.dom.addEventListener('keydown', event => this.keydown(event));

    const textArea = (name, label) => {
      const field = element('textarea', 'cm-textfield source-search-field');
      field.name = name;
      field.rows = 1;
      field.placeholder = label;
      field.setAttribute('aria-label', label);
      field.dataset.testid = name === 'search' ? 'source-search-input' : 'source-replace-input';
      field.spellcheck = false;
      field.setAttribute('autocapitalize', 'off');
      field.setAttribute('autocomplete', 'off');
      field.addEventListener('input', () => this.commit());
      return field;
    };
    const button = (name, label, command, title = label) => {
      const node = element('button', 'cm-button', label);
      node.type = 'button'; node.name = name; node.title = title;
      node.setAttribute('aria-label', title);
      node.dataset.testid = `source-search-${name}`;
      node.addEventListener('click', () => { this.commit(); command(view); });
      return node;
    };

    this.searchField = textArea('search', '検索する文字列');
    this.searchField.setAttribute('main-field', 'true');
    this.searchField.setAttribute('aria-describedby', 'source-search-help');
    this.nextButton = button('next', '↓', findNext, '次の一致箇所へ (Enter)');
    this.previousButton = button('previous', '↑', findPrevious, '前の一致箇所へ (Shift + Enter)');
    this.count = element('span', 'source-search-count');
    this.count.setAttribute('role', 'status');
    this.count.dataset.testid = 'source-search-count';
    const close = button('close', '×', closeSearchPanel, '検索を閉じる (Esc)');
    const first = element('div', 'source-search-row');
    first.append(this.searchField, this.count, this.previousButton, this.nextButton, close);

    const options = element('div', 'source-search-options');
    const checkbox = (name, label) => {
      const wrapper = element('label');
      const input = element('input'); input.type = 'checkbox'; input.name = name;
      input.addEventListener('change', () => this.commit());
      wrapper.append(input, document.createTextNode(label)); options.append(wrapper);
      return input;
    };
    this.caseField = checkbox('case', '大文字と小文字を区別');
    this.regexpField = checkbox('regexp', '正規表現');
    this.wordField = checkbox('word', '単語単位');
    const help = element('span', 'source-search-help', 'Ctrl + Enter で改行');
    help.id = 'source-search-help'; options.append(help);

    this.replaceField = textArea('replace', '置換後の文字列');
    this.replaceButton = button('replace', '置換', replaceNext);
    this.replaceAllButton = button('replace-all', 'すべて置換', replaceAll);
    const second = element('div', 'source-search-row source-replace-row');
    second.append(this.replaceField, this.replaceButton, this.replaceAllButton);
    this.dom.append(first, options, second);
    this.setQuery(this.query);
    this.refreshMatches();
  }

  commit() {
    this.resizeFields();
    const query = sourceQuery(this.searchField.value, {
      replace: this.replaceField.value, caseSensitive: this.caseField.checked,
      regexp: this.regexpField.checked, wholeWord: this.wordField.checked,
    });
    if (!query.eq(this.query) || !this.query.literal) {
      this.query = query;
      this.view.dispatch({ effects: setSearchQuery.of(query) });
    }
  }

  setQuery(query) {
    this.query = query;
    this.searchField.value = query.search;
    this.replaceField.value = query.replace;
    this.caseField.checked = query.caseSensitive;
    this.regexpField.checked = query.regexp;
    this.wordField.checked = query.wholeWord;
    this.resizeFields();
  }

  resizeFields() {
    for (const field of [this.searchField, this.replaceField]) {
      field.rows = Math.max(1, Math.min(6, field.value.split('\n').length));
    }
    this.view.requestMeasure();
  }

  refreshMatches() {
    clearTimeout(this.timer);
    this.matches = [];
    this.truncated = false;
    if (this.query.valid) {
      const cursor = this.query.getCursor(this.view.state);
      while (!cursor.next().done) {
        if (this.matches.length === 10_000) { this.truncated = true; break; }
        this.matches.push({ from: cursor.value.from, to: cursor.value.to });
      }
    }
    this.updateControls();
  }

  updateControls() {
    const selection = this.view.state.selection.main;
    const index = this.matches.findIndex(match => match.from === selection.from && match.to === selection.to);
    const total = `${this.matches.length}${this.truncated ? '+' : ''}`;
    this.count.textContent = !this.query.search ? '' : !this.query.valid ? '正規表現を確認' : index < 0 ? `${total} 件` : `${index + 1} / ${total}`;
    this.searchField.setAttribute('aria-invalid', String(!!this.query.search && !this.query.valid));
    this.nextButton.disabled = this.previousButton.disabled = !this.query.valid;
    this.replaceButton.disabled = this.replaceAllButton.disabled = !this.query.valid || this.view.state.readOnly;
    this.replaceField.disabled = this.view.state.readOnly;
  }

  keydown(event) {
    if (event.isComposing || this.view.composing || event.keyCode === 229) return;
    const isField = event.target === this.searchField || event.target === this.replaceField;
    if (event.key === 'Enter' && isField) {
      event.preventDefault(); event.stopPropagation();
      if (event.ctrlKey || event.metaKey || event.altKey) {
        const field = event.target;
        field.setRangeText('\n', field.selectionStart, field.selectionEnd, 'end');
        this.commit();
      } else {
        this.commit();
        (event.target === this.replaceField ? replaceNext : event.shiftKey ? findPrevious : findNext)(this.view);
      }
      return;
    }
    if (runScopeHandlers(this.view, event, 'search-panel')) event.preventDefault();
  }

  update(update) {
    let queryChanged = false;
    for (const transaction of update.transactions) {
      for (const effect of transaction.effects) {
        if (effect.is(setSearchQuery)) {
          queryChanged = true;
          if (!effect.value.eq(this.query)) this.setQuery(effect.value);
        }
      }
    }
    if (update.docChanged || queryChanged) {
      clearTimeout(this.timer);
      this.timer = setTimeout(() => this.refreshMatches(), 40);
    }
    this.updateControls();
  }

  mount() { this.searchField.focus(); this.searchField.select(); }
  destroy() { clearTimeout(this.timer); }
  get top() { return true; }
  get pos() { return 80; }
}
