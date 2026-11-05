import { PDFFindController, FindState } from 'pdfjs-dist/web/pdf_viewer.mjs';

// Use PDF.js's text extraction and text-layer highlights, including pages that
// have not yet been drawn. Searching does not allocate canvases for every page.
export class PDFSearch {
  constructor({ eventBus, linkService, pane, toolbar, scroll }) {
    this.eventBus = eventBus;
    this.scroll = scroll;
    this.enabled = false;
    this.composing = false;
    this.controller = new PDFFindController({ eventBus, linkService, delay: 200 });

    this.toggle = document.createElement('button');
    this.toggle.type = 'button';
    this.toggle.className = 'button button-subtle button-small pdf-search-toggle';
    this.toggle.textContent = 'PDF 内を検索';
    this.toggle.dataset.testid = 'pdf-search-toggle';
    this.toggle.title = 'PDF 内を検索 (PDF にフォーカスして Ctrl+F)';
    this.toggle.setAttribute('aria-expanded', 'false');
    this.toggle.setAttribute('aria-controls', 'pdf-search-bar');
    const title = pane.querySelector('.pane-title');
    title.insertBefore(this.toggle, title.lastElementChild);

    this.bar = document.createElement('div');
    this.bar.id = 'pdf-search-bar';
    this.bar.className = 'pdf-search-bar';
    this.bar.hidden = true;
    this.bar.setAttribute('role', 'search');
    this.bar.setAttribute('aria-label', 'PDF 内を検索');
    this.bar.innerHTML = `<div class="pdf-search-row">
      <input type="search" data-testid="pdf-search-input" aria-label="PDF 内で探す文字列" placeholder="PDF 内で検索" autocomplete="off" spellcheck="false">
      <button type="button" data-testid="pdf-search-prev" class="icon-button" aria-label="前の検索結果" title="前の結果 (Shift+Enter)">↑</button>
      <button type="button" data-testid="pdf-search-next" class="icon-button" aria-label="次の検索結果" title="次の結果 (Enter)">↓</button>
      <button type="button" class="icon-button" data-search-close aria-label="PDF の検索を閉じる" title="閉じる (Esc)">×</button>
    </div><div class="pdf-search-options"><label><input type="checkbox" data-search-case> 大文字と小文字を区別</label><span data-testid="pdf-search-count" class="pdf-search-count" role="status" aria-live="polite"></span></div>`;
    toolbar.after(this.bar);
    this.input = this.bar.querySelector('input[type="search"]');
    this.caseSensitive = this.bar.querySelector('[data-search-case]');
    this.count = this.bar.querySelector('.pdf-search-count');
    this.previous = this.bar.querySelector('[data-testid="pdf-search-prev"]');
    this.next = this.bar.querySelector('[data-testid="pdf-search-next"]');

    this.toggle.addEventListener('click', () => this.bar.hidden ? this.open() : this.close());
    this.bar.querySelector('[data-search-close]').addEventListener('click', () => this.close());
    this.previous.addEventListener('click', () => this.search('again', true));
    this.next.addEventListener('click', () => this.search('again', false));
    this.input.addEventListener('compositionstart', () => { this.composing = true; });
    this.input.addEventListener('compositionend', () => { this.composing = false; this.search(); });
    this.input.addEventListener('input', () => { if (!this.composing) this.search(); });
    this.caseSensitive.addEventListener('change', () => this.search('casesensitivitychange'));
    this.bar.addEventListener('keydown', event => {
      if (event.isComposing) return;
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); this.close(); }
      else if (event.key === 'Enter') { event.preventDefault(); this.search('again', event.shiftKey); }
    });
    pane.addEventListener('keydown', event => {
      if ((event.ctrlKey || event.metaKey) && !event.altKey && event.key.toLowerCase() === 'f' && this.enabled) {
        event.preventDefault(); event.stopPropagation(); this.open();
      } else if (event.key === 'F3' && this.enabled) {
        event.preventDefault(); this.open(false); this.search('again', event.shiftKey);
      }
    });
    scroll.addEventListener('pointerdown', event => {
      if (!event.target.closest('input, textarea, select, button, a')) scroll.focus({ preventScroll: true });
    });
    eventBus.on('updatefindcontrolstate', event => {
      if (event.source !== this.controller) return;
      this.input.classList.toggle('search-not-found', event.state === FindState.NOT_FOUND);
      if (!this.input.value) this.count.textContent = '';
      else if (event.state === FindState.PENDING) this.count.textContent = '検索しています…';
      else if (event.state === FindState.NOT_FOUND) this.count.textContent = '見つかりません';
      else this.showCount(event.matchesCount);
    });
    eventBus.on('updatefindmatchescount', event => { if (event.source === this.controller) this.showCount(event.matchesCount); });
    eventBus.on('pagesinit', () => this.reset());
    this.setEnabled(false);
  }

  setEnabled(enabled) {
    this.enabled = enabled;
    this.toggle.disabled = !enabled;
    this.input.disabled = !enabled;
    this.caseSensitive.disabled = !enabled;
    this.previous.disabled = this.next.disabled = !enabled || !this.input.value;
  }

  open(focus = true) {
    if (!this.enabled) return;
    this.bar.hidden = false;
    this.toggle.setAttribute('aria-expanded', 'true');
    if (focus) { this.input.focus(); this.input.select(); }
  }

  close(focus = true) {
    this.bar.hidden = true;
    this.toggle.setAttribute('aria-expanded', 'false');
    this.eventBus.dispatch('findbarclose', { source: this });
    if (focus) this.scroll.focus({ preventScroll: true });
  }

  reset() {
    this.close(false);
    this.input.value = '';
    this.input.classList.remove('search-not-found');
    this.count.textContent = '';
    this.setEnabled(this.enabled);
  }

  search(type = '', previous = false) {
    if (!this.enabled || this.composing) return;
    this.previous.disabled = this.next.disabled = !this.input.value;
    this.eventBus.dispatch('find', {
      source: this, type, query: this.input.value,
      caseSensitive: this.caseSensitive.checked,
      entireWord: false, highlightAll: true, findPrevious: previous, matchDiacritics: false,
    });
  }

  showCount(matches) {
    this.count.textContent = !this.input.value ? '' : matches?.total > 0 ? `${matches.current} / ${matches.total} 件` : '検索しています…';
  }
}
