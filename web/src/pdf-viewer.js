import * as pdfjs from 'pdfjs-dist/build/pdf.mjs';

pdfjs.GlobalWorkerOptions.workerSrc = '/pdf.worker.min.mjs';

const isCancelled = error => /RenderingCancelledException|AbortException/.test(error?.name || '') || /destroyed|cancelled/i.test(error?.message || '');

export class PDFViewer {
  constructor({ token, onError, onReady }) {
    this.token = token;
    this.onError = onError;
    this.onReady = onReady;
    this.document = null;
    this.loadingTask = null;
    this.renderTask = null;
    this.textLayer = null;
    this.lastPage = null;
    this.buildId = '';
    this.requestedBuildId = '';
    this.lastRequest = null;
    this.pageNumber = 1;
    this.generation = 0;
    this.renderGeneration = 0;
    this.zoom = 'fit';
    this.elements = Object.fromEntries(['scroll', 'stage', 'canvas', 'text-layer', 'empty', 'loading', 'prev', 'next', 'page', 'pages', 'zoom'].map(name => [name, document.getElementById(`pdf-${name}`)]));
    this.elements.prev.addEventListener('click', () => this.goToPage(this.pageNumber - 1));
    this.elements.next.addEventListener('click', () => this.goToPage(this.pageNumber + 1));
    this.elements.page.addEventListener('change', () => this.goToPage(Number(this.elements.page.value)));
    this.elements.page.addEventListener('keydown', event => {
      if (event.key === 'Enter') { event.preventDefault(); this.elements.page.blur(); }
    });
    this.elements.zoom.addEventListener('change', () => {
      this.zoom = this.elements.zoom.value;
      this.render(true).catch(error => this.report(error));
    });
    this.resizeTimer = null;
    let previousWidth = 0;
    this.resizeObserver = new ResizeObserver(entries => {
      const width = entries[0]?.contentRect.width || 0;
      if (Math.abs(width - previousWidth) < 1) return;
      previousWidth = width;
      clearTimeout(this.resizeTimer);
      this.resizeTimer = setTimeout(() => {
        if (this.document && this.zoom === 'fit') this.render(true).catch(error => this.report(error));
      }, 150);
    });
    this.resizeObserver.observe(this.elements.scroll);
  }

  updateControls() {
    const count = this.document?.numPages || 0;
    this.elements.page.value = String(this.pageNumber);
    this.elements.page.max = String(count || 1);
    this.elements.pages.textContent = count ? String(count) : '—';
    this.elements.prev.disabled = !count || this.pageNumber <= 1;
    this.elements.next.disabled = !count || this.pageNumber >= count;
    this.elements.page.disabled = !count;
    this.elements.zoom.disabled = !count;
  }

  async load(buildId, projectRoot) {
    if (!buildId || buildId === this.requestedBuildId) return;
    const generation = ++this.generation;
    const desiredPage = this.pageNumber;
    const scrollTop = this.elements.scroll.scrollTop;
    const scrollLeft = this.elements.scroll.scrollLeft;
    this.requestedBuildId = buildId;
    this.lastRequest = { buildId, projectRoot };
    this.elements.loading.hidden = false;
    const oldLoading = this.loadingTask;
    this.loadingTask = null;
    if (oldLoading && !this.document) await oldLoading.destroy().catch(() => {});
    if (generation !== this.generation) return;
    const task = pdfjs.getDocument({
      url: `/api/pdf?build=${encodeURIComponent(buildId)}`,
      httpHeaders: { 'X-Local-Token': this.token, 'X-Project-Root': encodeURIComponent(projectRoot) },
      cMapUrl: '/cmaps/',
      cMapPacked: true,
      standardFontDataUrl: '/standard_fonts/',
      wasmUrl: '/wasm/',
      isEvalSupported: false,
      enableXfa: false,
    });
    this.loadingTask = task;
    try {
      const nextDocument = await task.promise;
      if (generation !== this.generation) { await nextDocument.destroy(); return; }
      await this.cancelRender();
      if (generation !== this.generation) { await nextDocument.destroy(); return; }
      const previousDocument = this.document;
      this.document = nextDocument;
      this.buildId = buildId;
      this.pageNumber = Math.min(Math.max(desiredPage, 1), nextDocument.numPages);
      this.lastPage = null;
      this.updateControls();
      this.elements.empty.hidden = true;
      this.elements.stage.hidden = false;
      await previousDocument?.destroy();
      if (generation !== this.generation) return;
      await this.render(false);
      this.elements.scroll.scrollTop = scrollTop;
      this.elements.scroll.scrollLeft = scrollLeft;
      this.onReady?.(buildId, nextDocument.numPages);
    } catch (error) {
      if (generation !== this.generation || isCancelled(error)) return;
      if (task !== this.document?.loadingTask) await task.destroy().catch(() => {});
      if (!this.document) {
        this.elements.stage.hidden = true;
        this.elements.empty.hidden = false;
        this.elements.empty.querySelector('h3').textContent = 'PDF を表示できませんでした';
        this.elements.empty.querySelector('p').textContent = error?.message || String(error);
      }
      this.report(error);
    } finally {
      if (generation === this.generation) this.elements.loading.hidden = true;
    }
  }

  async cancelRender() {
    const generation = ++this.renderGeneration;
    this.textLayer?.cancel();
    this.textLayer = null;
    const task = this.renderTask;
    this.renderTask = null;
    if (task) {
      task.cancel();
      await task.promise.catch(() => {});
    }
    return generation;
  }

  async render(preserveScroll = false) {
    if (!this.document || this.elements.scroll.clientWidth < 50) return;
    const generation = await this.cancelRender();
    if (generation !== this.renderGeneration) return;
    const document = this.document;
    const buildId = this.buildId;
    const targetPage = this.pageNumber;
    const scroll = this.elements.scroll;
    const scrollTop = scroll.scrollTop;
    const scrollLeft = scroll.scrollLeft;
    const page = await document.getPage(targetPage);
    if (generation !== this.renderGeneration || document !== this.document) return;
    const base = page.getViewport({ scale: 1 });
    const availableWidth = Math.max(160, scroll.clientWidth - (window.innerWidth <= 640 ? 28 : 46));
    const scale = this.zoom === 'fit' ? availableWidth / base.width : Number(this.zoom);
    const viewport = page.getViewport({ scale: Math.max(0.1, scale) });
    // One page is drawn at a time. Cap pixel allocation on high-DPI displays.
    const outputScale = Math.min(window.devicePixelRatio || 1, 2, Math.sqrt(12_000_000 / (viewport.width * viewport.height)));
    const { canvas, stage } = this.elements;
    const container = this.elements['text-layer'];
    canvas.dataset.ready = 'false';
    canvas.width = Math.max(1, Math.floor(viewport.width * outputScale));
    canvas.height = Math.max(1, Math.floor(viewport.height * outputScale));
    canvas.style.width = `${viewport.width}px`;
    canvas.style.height = `${viewport.height}px`;
    canvas.setAttribute('aria-label', `PDF ${targetPage} / ${document.numPages} ページ`);
    stage.style.width = `${viewport.width}px`;
    stage.style.height = `${viewport.height}px`;
    stage.style.setProperty('--scale-factor', String(viewport.scale));
    stage.style.setProperty('--user-unit', String(viewport.userUnit || 1));
    stage.style.setProperty('--total-scale-factor', String(viewport.scale * (viewport.userUnit || 1)));
    container.replaceChildren();
    const task = page.render({
      canvasContext: canvas.getContext('2d', { alpha: false }),
      canvas,
      viewport,
      transform: outputScale === 1 ? null : [outputScale, 0, 0, outputScale, 0, 0],
      background: 'rgb(255,255,255)',
    });
    this.renderTask = task;
    try {
      await task.promise;
      if (generation !== this.renderGeneration || document !== this.document) return;
      canvas.dataset.ready = 'true';
      canvas.dataset.page = String(targetPage);
      canvas.dataset.buildId = buildId;
      if (this.lastPage && this.lastPage !== page) this.lastPage.cleanup();
      this.lastPage = page;
      this.updateControls();
      if (preserveScroll) { scroll.scrollTop = scrollTop; scroll.scrollLeft = scrollLeft; }
      const layer = new pdfjs.TextLayer({
        textContentSource: page.streamTextContent(),
        container,
        viewport,
      });
      this.textLayer = layer;
      layer.render().catch(error => {
        if (!isCancelled(error) && generation === this.renderGeneration) {
          // The PDF image remains useful if its optional selectable text fails.
          console.warn('PDF text layer:', error);
        }
      });
    } catch (error) {
      if (!isCancelled(error)) throw error;
    } finally {
      if (this.renderTask === task) this.renderTask = null;
    }
  }

  goToPage(value) {
    if (!this.document) return;
    const page = Math.min(this.document.numPages, Math.max(1, Number.isFinite(value) ? Math.round(value) : this.pageNumber));
    this.pageNumber = page;
    this.updateControls();
    this.elements.scroll.scrollTop = 0;
    this.elements.scroll.scrollLeft = 0;
    this.render().catch(error => this.report(error));
  }

  report(error) {
    if (!isCancelled(error)) this.onError?.(new Error(`PDF の表示に失敗しました．${error?.message || error}`));
  }

  async retry() {
    const request = this.lastRequest;
    if (!request) return;
    if (this.document && this.buildId === request.buildId) {
      await this.render(true);
      this.onReady?.(this.buildId, this.document.numPages);
    } else {
      this.requestedBuildId = '';
      await this.load(request.buildId, request.projectRoot);
    }
  }

  async reset() {
    ++this.generation;
    const loadingTask = this.loadingTask;
    const document = this.document;
    this.loadingTask = null;
    this.document = null;
    this.buildId = '';
    this.requestedBuildId = '';
    this.lastRequest = null;
    this.pageNumber = 1;
    await this.cancelRender();
    await document?.destroy().catch(() => {});
    if (loadingTask && loadingTask !== document?.loadingTask) await loadingTask.destroy().catch(() => {});
    this.lastPage = null;
    this.elements.stage.hidden = true;
    this.elements.empty.hidden = false;
    this.elements.empty.querySelector('h3').textContent = 'PDF をここに表示します';
    this.elements.empty.querySelector('p').textContent = '原稿を保存し，コンパイルしてください．';
    this.elements.loading.hidden = true;
    this.elements.canvas.width = 0;
    this.elements.canvas.height = 0;
    this.elements.canvas.dataset.ready = 'false';
    this.elements['text-layer'].replaceChildren();
    this.updateControls();
  }
}
