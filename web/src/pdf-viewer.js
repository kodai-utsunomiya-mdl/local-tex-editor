import * as pdfjs from 'pdfjs-dist/build/pdf.mjs';
import { EventBus, PDFViewer as DocumentViewer, PDFLinkService, LinkTarget, RenderingStates } from 'pdfjs-dist/web/pdf_viewer.mjs';
import { PDFSearch } from './pdf-search.js';

pdfjs.GlobalWorkerOptions.workerSrc = '/pdf.worker.min.mjs';
const cancelled = error => /RenderingCancelledException|AbortException/.test(error?.name || '') || /destroyed|cancelled/i.test(error?.message || '');
const clamp = (value, low, high) => Math.min(high, Math.max(low, value));

export class PDFViewer {
  constructor({ token, onError, onReady, onPositionChange, onInverse }) {
    this.token = token;
    this.onError = onError;
    this.onReady = onReady;
    this.onPositionChange = onPositionChange;
    this.onInverse = onInverse;
    this.document = null;
    this.loadingTask = null;
    this.buildId = '';
    this.requestedBuildId = '';
    this.lastRequest = null;
    this.generation = 0;
    this.ready = false;
    this.hasRendered = false;
    this.zoomSetting = 'page-width';
    this.initialPosition = null;
    this.pendingPosition = null;
    this.lastLocation = null;
    this.currentCanvas = null;
    this.highlight = null;
    this.highlightTimer = null;
    this.elements = Object.fromEntries(['scroll', 'stage', 'empty', 'loading', 'prev', 'next', 'page', 'pages', 'zoom', 'zoom-in', 'zoom-out'].map(name => [name, document.getElementById(`pdf-${name}`)]));
    this.eventBus = new EventBus();
    this.linkService = new PDFLinkService({ eventBus: this.eventBus, externalLinkTarget: LinkTarget.BLANK, externalLinkRel: 'noopener noreferrer', ignoreDestinationZoom: true });
    this.search = new PDFSearch({ eventBus: this.eventBus, linkService: this.linkService, pane: document.getElementById('pdf-pane'), toolbar: document.querySelector('.pdf-toolbar'), scroll: this.elements.scroll });
    this.pageViewer = new DocumentViewer({
      container: this.elements.scroll,
      viewer: this.elements.stage,
      eventBus: this.eventBus,
      linkService: this.linkService,
      findController: this.search.controller,
      annotationEditorMode: pdfjs.AnnotationEditorType.DISABLE,
      annotationMode: pdfjs.AnnotationMode.ENABLE,
      imageResourcesPath: '/images/',
      maxCanvasPixels: 3_000_000,
      maxCanvasDim: 8192,
      enableDetailCanvas: true,
      enableAutoLinking: false,
      minDurationToUpdateCanvas: 100,
    });
    this.linkService.setViewer(this.pageViewer);
    this.pageViewer.renderingQueue.onIdle = () => this.pageViewer.cleanup();
    this.eventBus.on('pagesinit', () => this.initialisePages().catch(error => this.report(error)));
    this.eventBus.on('pagechanging', () => { this.updateControls(); this.markCurrentCanvas(); });
    this.eventBus.on('scalechanging', event => {
      this.zoomSetting = event.presetValue || String(event.scale);
      this.updateControls(); this.drawHighlight();
    });
    this.eventBus.on('pagerender', event => {
      if (event.source === this.pageViewer.getPageView(event.pageNumber - 1) && event.source.canvas) event.source.canvas.dataset.ready = 'false';
    });
    this.eventBus.on('pagerendered', event => this.pageRendered(event));
    this.eventBus.on('updateviewarea', event => {
      if (!this.ready || !this.document) return;
      this.lastLocation = { ...event.location };
      this.markCurrentCanvas();
      this.onPositionChange?.(this.getViewState());
    });
    this.elements.prev.addEventListener('click', () => this.goToPage(this.pageNumber - 1));
    this.elements.next.addEventListener('click', () => this.goToPage(this.pageNumber + 1));
    this.elements.page.addEventListener('change', () => this.goToPage(Number(this.elements.page.value)));
    this.elements.page.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); this.elements.page.blur(); } });
    this.elements.zoom.addEventListener('change', () => {
      if (!this.ready) return;
      this.zoomSetting = this.elements.zoom.value;
      this.pageViewer.currentScaleValue = this.zoomSetting;
    });
    this.elements['zoom-in'].addEventListener('click', () => this.changeZoom(1));
    this.elements['zoom-out'].addEventListener('click', () => this.changeZoom(-1));
    this.elements.scroll.addEventListener('wheel', event => {
      if (!this.ready || !event.ctrlKey) return;
      event.preventDefault();
      const delta = event.deltaY * (event.deltaMode === 1 ? 30 : event.deltaMode === 2 ? this.elements.scroll.clientHeight : 1);
      this.pageViewer.updateScale({ scaleFactor: clamp(Math.exp(-delta * 0.002), 0.6, 1.5), origin: [event.clientX, event.clientY], drawingDelay: 150 });
    }, { passive: false });
    let pinch = null;
    const touchPair = touches => ({ distance: Math.hypot(touches[0].clientX - touches[1].clientX, touches[0].clientY - touches[1].clientY), origin: [(touches[0].clientX + touches[1].clientX) / 2, (touches[0].clientY + touches[1].clientY) / 2] });
    this.elements.scroll.addEventListener('touchstart', event => { if (this.ready && event.touches.length === 2) { event.preventDefault(); pinch = touchPair(event.touches); } }, { passive: false });
    this.elements.scroll.addEventListener('touchmove', event => {
      if (!this.ready || event.touches.length !== 2) return;
      event.preventDefault(); const next = touchPair(event.touches);
      if (pinch?.distance > 0) this.pageViewer.updateScale({ scaleFactor: next.distance / pinch.distance, origin: next.origin, drawingDelay: 150 });
      pinch = next;
    }, { passive: false });
    this.elements.scroll.addEventListener('touchend', () => { pinch = null; });
    this.elements.scroll.addEventListener('dblclick', event => this.inverseAt(event));
    let resizeTimer;
    this.resizeObserver = new ResizeObserver(() => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        this.refreshLayout();
      }, 100);
    });
    this.resizeObserver.observe(this.elements.scroll);
    this.updateControls();
  }

  get pageNumber() { return this.pageViewer.currentPageNumber || 1; }

  updateControls() {
    const count = this.document?.numPages || 0;
    const enabled = !!count && this.ready;
    this.search.setEnabled(enabled);
    this.elements.page.value = String(this.pageNumber);
    this.elements.page.max = String(count || 1);
    this.elements.pages.textContent = count ? String(count) : '—';
    this.elements.prev.disabled = !enabled || this.pageNumber <= 1;
    this.elements.next.disabled = !enabled || this.pageNumber >= count;
    this.elements.page.disabled = !enabled;
    this.elements.zoom.disabled = !enabled;
    this.elements['zoom-in'].disabled = !enabled || this.pageViewer.currentScale >= 25;
    this.elements['zoom-out'].disabled = !enabled || this.pageViewer.currentScale <= 0.1;
    const select = this.elements.zoom;
    const custom = select.querySelector('[data-custom-zoom]');
    const scale = this.pageViewer.currentScale;
    const preset = [...select.options].find(option => option !== custom && option.value === this.zoomSetting);
    if (preset) { custom.hidden = true; select.value = preset.value; }
    else { custom.hidden = false; custom.value = String(scale); custom.textContent = `${Math.round(scale * 100)}%`; select.value = custom.value; }
  }

  restorePosition(position) {
    this.initialPosition = position && typeof position === 'object' ? position : null;
  }

  getViewState() {
    if (!this.document || !this.ready) return this.initialPosition;
    const location = this.lastLocation;
    return { pageNumber: location?.pageNumber || this.pageNumber, left: location?.left ?? 0, top: location?.top ?? null, zoom: this.zoomSetting };
  }

  async load(buildId, projectRoot) {
    if (!buildId || buildId === this.requestedBuildId) return;
    const generation = ++this.generation;
    this.requestedBuildId = buildId;
    this.lastRequest = { buildId, projectRoot };
    this.elements.loading.hidden = false;
    const oldPending = this.loadingTask;
    this.loadingTask = null;
    if (oldPending && oldPending !== this.document?.loadingTask) await oldPending.destroy().catch(() => {});
    if (generation !== this.generation) return;
    let task;
    try {
      task = pdfjs.getDocument({
        url: `/api/pdf?build=${encodeURIComponent(buildId)}`,
        httpHeaders: { 'X-Local-Token': this.token, 'X-Project-Root': encodeURIComponent(projectRoot) },
        cMapUrl: '/cmaps/', cMapPacked: true,
        standardFontDataUrl: '/standard_fonts/', wasmUrl: '/wasm/',
        isEvalSupported: false, enableXfa: false,
        disableAutoFetch: true,
      });
      this.loadingTask = task;
      const nextDocument = await task.promise;
      await nextDocument.getPage(1);
      if (generation !== this.generation) { await task.destroy(); return; }
      const previous = this.document;
      this.pendingPosition = this.getViewState() || this.initialPosition;
      this.document = nextDocument;
      this.buildId = buildId;
      this.ready = false;
      this.hasRendered = false;
      this.lastLocation = null;
      this.currentCanvas = null;
      this.clearHighlight();
      this.elements.stage.hidden = false;
      this.elements.empty.hidden = true;
      this.linkService.setDocument(nextDocument);
      this.pageViewer.setDocument(nextDocument);
      this.pageViewer.pagesPromise?.catch(error => { if (generation === this.generation) this.report(error); });
      this.updateControls();
      await previous?.loadingTask.destroy();
    } catch (error) {
      if (generation !== this.generation || cancelled(error)) return;
      if (task && task !== this.document?.loadingTask) await task.destroy().catch(() => {});
      this.elements.loading.hidden = true;
      if (!this.document) {
        this.elements.stage.hidden = true; this.elements.empty.hidden = false;
        this.elements.empty.querySelector('h3').textContent = 'PDF を表示できませんでした';
        this.elements.empty.querySelector('p').textContent = error?.message || String(error);
      }
      this.report(error);
    }
  }

  async initialisePages() {
    const document = this.document;
    const generation = this.generation;
    if (!document || this.pageViewer.pdfDocument !== document) return;
    const position = this.pendingPosition;
    const number = clamp(Math.round(Number(position?.pageNumber) || 1), 1, document.numPages);
    const page = await document.getPage(number);
    if (generation !== this.generation || document !== this.document) return;
    const view = this.pageViewer.getPageView(number - 1);
    if (!view.pdfPage) view.setPdfPage(page);
    this.pageViewer.currentPageNumber = number;
    this.zoomSetting = position?.zoom || this.zoomSetting || 'page-width';
    if (this.elements.scroll.clientWidth >= 50 || !['page-width', 'page-fit'].includes(this.zoomSetting)) this.pageViewer.currentScaleValue = this.zoomSetting;
    if (Number.isFinite(position?.left) && Number.isFinite(position?.top)) {
      this.pageViewer.scrollPageIntoView({ pageNumber: number, destArray: [null, { name: 'XYZ' }, position.left, position.top, null], allowNegativeOffset: true, ignoreDestinationZoom: true });
    }
    this.ready = true;
    this.pendingPosition = null;
    this.pageViewer.update();
    this.updateControls();
    this.markCurrentCanvas();
    if (view.renderingState === RenderingStates.FINISHED && view.canvas) this.notifyReady();
  }

  pageRendered(event) {
    const view = this.pageViewer.getPageView(event.pageNumber - 1);
    if (!view || event.source !== view || this.pageViewer.pdfDocument !== this.document) return;
    if (event.error) { this.elements.loading.hidden = true; this.report(event.error); return; }
    const canvas = view.canvas;
    if (canvas) {
      canvas.dataset.ready = 'true';
      canvas.dataset.page = String(event.pageNumber);
      canvas.dataset.buildId = this.buildId;
    }
    this.markCurrentCanvas();
    this.drawHighlight();
    if (this.ready && event.pageNumber === this.pageNumber) this.notifyReady();
  }

  markCurrentCanvas() {
    const canvas = this.pageViewer.getPageView(this.pageNumber - 1)?.canvas;
    if (this.currentCanvas && this.currentCanvas !== canvas) this.currentCanvas.removeAttribute('data-testid');
    this.currentCanvas = canvas || null;
    if (canvas) canvas.dataset.testid = 'pdf-canvas';
  }

  notifyReady() {
    this.elements.loading.hidden = true;
    this.hasRendered = true;
    this.onReady?.(this.buildId, this.document?.numPages || 0);
  }

  changeZoom(direction) {
    if (!this.ready) return;
    const rect = this.elements.scroll.getBoundingClientRect();
    this.pageViewer.updateScale({ steps: direction, origin: [rect.left + rect.width / 2, rect.top + rect.height / 2], drawingDelay: 150 });
  }

  refreshLayout() {
    if (!this.ready || this.elements.scroll.clientWidth < 50) return;
    if (this.zoomSetting === 'page-width' || this.zoomSetting === 'page-fit') this.pageViewer.currentScaleValue = this.zoomSetting;
    this.pageViewer.update();
  }

  goToPage(value) {
    if (!this.document || !this.ready) return;
    this.pageViewer.currentPageNumber = clamp(Math.round(Number(value) || this.pageNumber), 1, this.document.numPages);
    this.pageViewer.update();
    this.updateControls(); this.markCurrentCanvas();
  }

  inverseAt(event) {
    if (!this.ready || !this.document || event.target.closest('input, textarea, select, button')) return;
    const element = event.target.closest('.page[data-page-number]');
    if (!element) return;
    const number = Number(element.dataset.pageNumber);
    const view = this.pageViewer.getPageView(number - 1);
    if (!view?.pdfPage) return;
    const rect = (element.querySelector('.canvasWrapper') || element).getBoundingClientRect();
    const x = (event.clientX - rect.left) * view.viewport.width / rect.width;
    const y = (event.clientY - rect.top) * view.viewport.height / rect.height;
    const point = view.viewport.convertToPdfPoint(x, y);
    const bounds = view.pdfPage.view;
    event.preventDefault();
    this.onInverse?.({ buildId: this.buildId, page: number, x: point[0] - bounds[0], y: bounds[3] - point[1] });
  }

  async showLocation(location, buildId = this.buildId) {
    if (!this.document || !this.ready || buildId !== this.buildId) return false;
    const document = this.document;
    const number = clamp(Math.round(Number(location.page) || 1), 1, document.numPages);
    const page = await document.getPage(number);
    if (document !== this.document || buildId !== this.buildId) return false;
    const view = this.pageViewer.getPageView(number - 1);
    if (!view.pdfPage) view.setPdfPage(page);
    const bounds = page.view;
    const x = bounds[0] + location.x;
    const y = bounds[3] - location.y;
    this.pageViewer.scrollPageIntoView({ pageNumber: number, destArray: [null, { name: 'XYZ' }, x, y, null], ignoreDestinationZoom: true, center: 'vertical' });
    this.pageViewer.update();
    this.clearHighlight();
    this.highlight = { ...location, page: number, buildId };
    this.drawHighlight();
    this.highlightTimer = setTimeout(() => this.clearHighlight(), 2800);
    return true;
  }

  drawHighlight() {
    const location = this.highlight;
    if (!location || location.buildId !== this.buildId) return;
    const view = this.pageViewer.getPageView(location.page - 1);
    if (!view?.pdfPage) return;
    let mark = view.div.querySelector('.synctex-highlight');
    if (!mark) { mark = document.createElement('div'); mark.className = 'synctex-highlight'; mark.dataset.testid = 'synctex-highlight'; mark.setAttribute('aria-hidden', 'true'); view.div.append(mark); }
    const bounds = view.pdfPage.view;
    const x = bounds[0] + location.x;
    const y = bounds[3] - location.y;
    const box = view.viewport.convertToViewportRectangle([x, y, x + Math.max(4, location.width || 0), y - Math.max(4, location.height || 0)]);
    mark.style.left = `${Math.min(box[0], box[2]) - 4}px`;
    mark.style.top = `${Math.min(box[1], box[3]) - 3}px`;
    mark.style.width = `${Math.abs(box[2] - box[0]) + 8}px`;
    mark.style.height = `${Math.abs(box[3] - box[1]) + 6}px`;
  }

  clearHighlight() {
    clearTimeout(this.highlightTimer);
    this.highlight = null;
    for (const mark of this.elements.stage.querySelectorAll('.synctex-highlight')) mark.remove();
  }

  report(error) {
    if (!cancelled(error)) {
      this.elements.loading.hidden = true;
      this.onError?.(new Error(`PDF の表示に失敗しました．${error?.message || error}`));
    }
  }

  async retry() {
    const request = this.lastRequest;
    if (!request) return;
    this.requestedBuildId = '';
    await this.load(request.buildId, request.projectRoot);
  }

  async reset() {
    ++this.generation;
    const loadingTask = this.loadingTask;
    const document = this.document;
    this.loadingTask = null; this.document = null;
    this.buildId = ''; this.requestedBuildId = ''; this.lastRequest = null;
    this.ready = false; this.hasRendered = false; this.initialPosition = null; this.pendingPosition = null; this.lastLocation = null; this.currentCanvas = null;
    this.clearHighlight();
    this.search.reset();
    this.pageViewer.setDocument(null); this.linkService.setDocument(null);
    this.elements.stage.hidden = true; this.elements.empty.hidden = false;
    this.elements.empty.querySelector('h3').textContent = 'PDF をここに表示します';
    this.elements.empty.querySelector('p').textContent = '原稿を保存し，コンパイルしてください．';
    this.elements.loading.hidden = true;
    this.updateControls();
    await document?.loadingTask.destroy().catch(() => {});
    if (loadingTask && loadingTask !== document?.loadingTask) await loadingTask.destroy().catch(() => {});
  }
}
