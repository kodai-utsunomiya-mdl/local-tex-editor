import * as pdfjs from 'pdfjs-dist/build/pdf.mjs';
import { EventBus, PDFViewer, PDFLinkService, LinkTarget } from 'pdfjs-dist/web/pdf_viewer.mjs';

pdfjs.GlobalWorkerOptions.workerSrc = '/pdf.worker.min.mjs';
const clamp = (value, low, high) => Math.min(high, Math.max(low, value));
const bytes = value => value < 1024 ? `${value} B` : value < 1024 ** 2 ? `${(value / 1024).toFixed(1)} KB` : `${(value / 1024 ** 2).toFixed(1)} MB`;

// The selected image or PDF has its own view. Source documents stay in the
// editor's state map, and decoded assets are released when their tab is left.
export class AssetPreview {
  constructor({ token }) {
    this.token = token;
    this.root = document.getElementById('asset-preview');
    this.elements = Object.fromEntries(['info', 'page-controls', 'prev', 'next', 'page', 'pages', 'zoom-out', 'zoom-in', 'zoom', 'reload', 'scroll', 'image-stage', 'image', 'pdf-stage', 'message'].map(id => [id, document.getElementById(`asset-${id}`)]));
    this.file = null; this.projectRoot = ''; this.generation = 0;
    this.controller = null; this.objectURL = null; this.loadingTask = null; this.document = null;
    this.ready = false; this.scale = 1; this.zoomSetting = 'page-fit'; this.location = null;
    this.elements['pdf-stage'].dataset.testid = 'asset-pdf';
    this.elements['zoom-in'].addEventListener('click', () => this.changeZoom(1.2));
    this.elements['zoom-out'].addEventListener('click', () => this.changeZoom(1 / 1.2));
    this.elements.zoom.addEventListener('change', () => { this.zoomSetting = this.elements.zoom.value; this.applyZoom(); });
    this.elements.prev.addEventListener('click', () => this.goToPage((this.pdfViewer?.currentPageNumber || 1) - 1));
    this.elements.next.addEventListener('click', () => this.goToPage((this.pdfViewer?.currentPageNumber || 1) + 1));
    this.elements.page.addEventListener('change', () => this.goToPage(Number(this.elements.page.value)));
    this.elements.reload.addEventListener('click', () => { if (this.file) this.show(this.file, this.projectRoot); });
    this.elements.scroll.addEventListener('wheel', event => {
      if (!event.ctrlKey || !this.ready) return;
      event.preventDefault();
      const delta = event.deltaY * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? this.elements.scroll.clientHeight : 1);
      this.changeZoom(clamp(Math.exp(-delta * .002), .6, 1.5), [event.clientX, event.clientY]);
    }, { passive: false });
    this.resizeObserver = new ResizeObserver(() => {
      if (this.ready && !this.root.hidden && ['page-fit', 'page-width'].includes(this.zoomSetting)) this.applyZoom();
    });
    this.resizeObserver.observe(this.elements.scroll);
    this.updateControls();
  }

  savePosition() {
    if (!this.file || !this.ready) return;
    this.file.preview = {
      zoom: this.zoomSetting,
      page: this.pdfViewer?.currentPageNumber || 1,
      left: this.location?.left,
      top: this.location?.top,
      scrollTop: this.elements.scroll.scrollTop,
      scrollLeft: this.elements.scroll.scrollLeft,
    };
  }

  hide() {
    this.savePosition(); ++this.generation; this.ready = false;
    this.root.hidden = true; this.root.dataset.state = 'idle';
    this.controller?.abort(); this.controller = null;
    this.pdfViewer?.setDocument(null); this.linkService?.setDocument(null);
    const task = this.loadingTask; this.loadingTask = null; this.document = null;
    task?.destroy().catch(() => {});
    this.elements.image.removeAttribute('src'); this.elements.image.dataset.ready = 'false';
    if (this.objectURL) URL.revokeObjectURL(this.objectURL);
    this.objectURL = null; this.file = null; this.location = null;
    this.elements['pdf-stage'].dataset.ready = 'false';
  }

  async show(file, projectRoot) {
    this.hide();
    const generation = this.generation;
    this.file = file; this.projectRoot = projectRoot;
    this.root.hidden = false; this.root.dataset.path = file.path; this.root.dataset.kind = file.kind; this.root.dataset.state = 'loading';
    this.zoomSetting = file.preview?.zoom || 'page-fit'; this.scale = 1;
    this.elements.scroll.scrollTo(0, 0);
    this.elements['image-stage'].hidden = true; this.elements['pdf-stage'].hidden = true;
    this.elements['pdf-stage'].dataset.path = file.path;
    this.elements.info.textContent = bytes(file.size || 0); this.elements.info.title = file.path;
    this.message('読み込んでいます…'); this.updateControls();
    if (!['image', 'pdf'].includes(file.kind)) {
      this.root.dataset.state = 'unsupported';
      this.message(`${file.path}\n${bytes(file.size || 0)}\n\nこの種類のファイルはプレビューに対応していません．`);
      return;
    }
    try {
      const url = `/api/asset?path=${encodeURIComponent(file.path)}`;
      const headers = { 'X-Local-Token': this.token, 'X-Project-Root': encodeURIComponent(projectRoot) };
      if (file.kind === 'image') {
        const controller = new AbortController(); this.controller = controller;
        const response = await fetch(url, { headers, signal: controller.signal, cache: 'no-store' });
        if (!response.ok) {
          const text = await response.text(); let message = text;
          try { message = JSON.parse(text).error || text; } catch { /* Show the HTTP response when it is not JSON. */ }
          throw new Error(message || `画像を取得できませんでした (HTTP ${response.status})．`);
        }
        const blob = await response.blob();
        if (generation !== this.generation) return;
        this.objectURL = URL.createObjectURL(blob);
        const image = this.elements.image; image.alt = file.path; image.dataset.path = file.path; image.src = this.objectURL;
        await image.decode();
        if (generation !== this.generation) return;
        this.elements['image-stage'].hidden = false; this.ready = true;
        this.applyZoom();
        this.elements.scroll.scrollLeft = file.preview?.scrollLeft || 0; this.elements.scroll.scrollTop = file.preview?.scrollTop || 0;
        this.elements.info.textContent = `${image.naturalWidth} × ${image.naturalHeight} px · ${bytes(blob.size)}`;
        image.dataset.ready = 'true'; this.root.dataset.state = 'ready'; this.message('');
        this.updateControls();
      } else {
        this.ensurePDFViewer(); this.elements['pdf-stage'].hidden = false;
        const task = pdfjs.getDocument({
          url, httpHeaders: headers, cMapUrl: '/cmaps/', cMapPacked: true,
          standardFontDataUrl: '/standard_fonts/', wasmUrl: '/wasm/',
          isEvalSupported: false, enableXfa: false, disableAutoFetch: true, disableStream: true,
        });
        this.loadingTask = task;
        const document = await task.promise;
        await document.getPage(1);
        if (generation !== this.generation) return;
        this.document = document; this.linkService.setDocument(document); this.pdfViewer.setDocument(document);
        this.pdfViewer.pagesPromise?.catch(error => { if (generation === this.generation) this.fail(error); });
      }
    } catch (error) { if (generation === this.generation && error?.name !== 'AbortError') this.fail(error); }
  }

  ensurePDFViewer() {
    if (this.pdfViewer) return;
    this.eventBus = new EventBus();
    this.linkService = new PDFLinkService({ eventBus: this.eventBus, externalLinkTarget: LinkTarget.BLANK, externalLinkRel: 'noopener noreferrer', ignoreDestinationZoom: true });
    this.pdfViewer = new PDFViewer({
      container: this.elements.scroll, viewer: this.elements['pdf-stage'],
      eventBus: this.eventBus, linkService: this.linkService,
      annotationEditorMode: pdfjs.AnnotationEditorType.DISABLE,
      imageResourcesPath: '/images/', maxCanvasPixels: 3_000_000, maxCanvasDim: 8192,
      enableDetailCanvas: true, enableAutoLinking: false, minDurationToUpdateCanvas: 100,
    });
    this.linkService.setViewer(this.pdfViewer);
    this.pdfViewer.renderingQueue.onIdle = () => this.pdfViewer.cleanup();
    this.eventBus.on('pagesinit', () => { const generation = this.generation; this.initialisePDF().catch(error => { if (generation === this.generation) this.fail(error); }); });
    this.eventBus.on('pagechanging', () => this.updateControls());
    this.eventBus.on('scalechanging', event => { this.zoomSetting = event.presetValue || String(event.scale); this.scale = event.scale; this.updateControls(); });
    this.eventBus.on('updateviewarea', event => { if (this.document && this.ready) this.location = { ...event.location }; });
    this.eventBus.on('pagerendered', event => {
      const view = this.pdfViewer.getPageView(event.pageNumber - 1);
      if (!this.document || event.source !== view || this.pdfViewer.pdfDocument !== this.document) return;
      if (event.error) { this.fail(event.error); return; }
      if (view.canvas) { view.canvas.dataset.page = String(event.pageNumber); view.canvas.dataset.path = this.file.path; view.canvas.dataset.ready = 'true'; }
      this.elements['pdf-stage'].dataset.ready = 'true'; this.root.dataset.state = 'ready'; this.message('');
    });
  }

  async initialisePDF() {
    const document = this.document; const generation = this.generation;
    if (!document || this.pdfViewer.pdfDocument !== document) return;
    const position = this.file.preview;
    const number = clamp(Math.round(Number(position?.page) || 1), 1, document.numPages);
    const page = await document.getPage(number);
    if (generation !== this.generation || document !== this.document) return;
    const view = this.pdfViewer.getPageView(number - 1); if (!view.pdfPage) view.setPdfPage(page);
    this.pdfViewer.currentPageNumber = number; this.ready = true; this.applyZoom();
    if (Number.isFinite(position?.left) && Number.isFinite(position?.top)) this.pdfViewer.scrollPageIntoView({ pageNumber: number, destArray: [null, { name: 'XYZ' }, position.left, position.top, null], allowNegativeOffset: true, ignoreDestinationZoom: true });
    this.pdfViewer.update(); this.updateControls();
  }

  message(text, error = false) {
    this.elements.message.textContent = text; this.elements.message.hidden = !text;
    this.elements.message.classList.toggle('error', error);
    this.elements.message.setAttribute('role', error ? 'alert' : 'status');
  }

  fail(error) {
    if (this.root.hidden || !this.file) return;
    this.ready = false; this.root.dataset.state = 'error';
    this.message(`${this.file.path} を表示できませんでした．\n${error?.message || error}`, true);
    this.updateControls();
  }

  goToPage(value) {
    if (!this.document || !this.ready) return;
    this.pdfViewer.currentPageNumber = clamp(Math.round(value) || 1, 1, this.document.numPages);
    this.updateControls();
  }

  applyZoom(origin = null) {
    if (!this.ready || this.elements.scroll.clientWidth < 50) return;
    if (this.file.kind === 'pdf') {
      this.pdfViewer.currentScaleValue = this.zoomSetting;
      this.pdfViewer.update();
    } else {
      const image = this.elements.image; const scroll = this.elements.scroll;
      const rectangle = image.getBoundingClientRect();
      const point = origin && rectangle.width > 0 ? [(origin[0] - rectangle.left) / rectangle.width, (origin[1] - rectangle.top) / rectangle.height] : null;
      const widthFit = Math.max(1, scroll.clientWidth - 48) / image.naturalWidth;
      const heightFit = Math.max(1, scroll.clientHeight - 48) / image.naturalHeight;
      this.scale = this.zoomSetting === 'page-fit' ? Math.min(1, widthFit, heightFit) : this.zoomSetting === 'page-width' ? widthFit : clamp(Number(this.zoomSetting) || 1, .05, 8);
      image.style.width = `${image.naturalWidth * this.scale}px`; image.style.height = `${image.naturalHeight * this.scale}px`;
      if (point) { const next = image.getBoundingClientRect(); scroll.scrollLeft += next.left + point[0] * next.width - origin[0]; scroll.scrollTop += next.top + point[1] * next.height - origin[1]; }
    }
    this.updateControls();
  }

  changeZoom(factor, origin = null) {
    if (!this.ready) return;
    const bounds = this.elements.scroll.getBoundingClientRect();
    origin ||= [bounds.left + bounds.width / 2, bounds.top + bounds.height / 2];
    if (this.file.kind === 'pdf') this.pdfViewer.updateScale({ scaleFactor: factor, origin, drawingDelay: 150 });
    else { this.zoomSetting = String(clamp(this.scale * factor, .05, 8)); this.applyZoom(origin); }
  }

  updateControls() {
    const pdf = this.file?.kind === 'pdf';
    this.elements['page-controls'].hidden = !pdf;
    for (const id of ['zoom', 'zoom-out', 'zoom-in']) this.elements[id].disabled = !this.ready;
    this.elements.reload.disabled = !this.file || !['image', 'pdf'].includes(this.file.kind);
    const number = this.pdfViewer?.currentPageNumber || 1;
    this.elements.page.value = String(number); this.elements.page.max = String(this.document?.numPages || 1);
    this.elements.page.disabled = !this.ready;
    this.elements.pages.textContent = String(this.document?.numPages || '—');
    this.elements.prev.disabled = !this.ready || number <= 1; this.elements.next.disabled = !this.ready || number >= (this.document?.numPages || 1);
    const select = this.elements.zoom; const custom = select.querySelector('[data-custom-zoom]');
    const preset = [...select.options].some(option => option !== custom && option.value === this.zoomSetting);
    custom.hidden = preset;
    if (preset) select.value = this.zoomSetting;
    else { custom.value = this.zoomSetting; custom.textContent = `${Math.round(this.scale * 100)}%`; select.value = this.zoomSetting; }
  }
}
