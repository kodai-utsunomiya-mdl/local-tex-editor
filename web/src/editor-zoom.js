const clampSize = value => Math.min(40, Math.max(8, value));
const FONT_MEASURE = Symbol('editor-font-measure');

// Keep the text under the pointer at the same screen position after the
// editor has measured the new font. No document or selection is dispatched.
export function prepareEditorResize(view, origin = null) {
  if (!view || !view.dom.isConnected || !view.scrollDOM.clientHeight) return () => {};
  const scroll = view.scrollDOM;
  const bounds = scroll.getBoundingClientRect();
  const x = Math.min(bounds.right - 4, Math.max(bounds.left + 4, origin?.[0] ?? bounds.left + bounds.width / 2));
  const y = Math.min(bounds.bottom - 4, Math.max(bounds.top + 4, origin?.[1] ?? bounds.top + Math.min(60, bounds.height / 3)));
  const position = view.posAtCoords({ x, y }) ?? view.lineBlockAtHeight(Math.max(0, y - view.documentTop)).from;
  const before = view.coordsAtPos(position);
  const document = view.state.doc;
  return () => view.requestMeasure({
    key: FONT_MEASURE,
    read: () => view.dom.isConnected && view.state.doc === document ? view.coordsAtPos(position) : null,
    write: after => {
      if (!before || !after || view.state.doc !== document) return;
      scroll.scrollTop += after.top - before.top;
      scroll.scrollLeft += after.left - before.left;
    },
  });
}

export function attachEditorZoom(view, { getSize, onSize, isActive = () => true }) {
  const element = view.scrollDOM;
  const listeners = [];
  let frame = null; let pending = null;
  let wheelSize = getSize(); let appliedSize = wheelSize; let lastWheel = 0;
  let touch = null; let gesture = null; let initialTouch = null; let pinching = false;
  const add = (type, handler, capture = false) => { element.addEventListener(type, handler, { passive: false, capture }); listeners.push([type, handler, capture]); };
  const active = () => isActive() && !view.composing;
  const queue = (size, origin) => {
    pending = { size: clampSize(size), origin };
    if (frame !== null) return;
    frame = requestAnimationFrame(() => {
      frame = null;
      if (active() && pending) { onSize(pending.size, pending.origin); appliedSize = getSize(); wheelSize = pending.size; }
      pending = null;
    });
  };
  const touchPosition = touches => {
    const first = touches[0], second = touches[1];
    return {
      distance: Math.hypot(second.clientX - first.clientX, second.clientY - first.clientY),
      origin: [(first.clientX + second.clientX) / 2, (first.clientY + second.clientY) / 2],
    };
  };
  add('wheel', event => {
    if (!(event.ctrlKey || event.metaKey) || !isActive()) return;
    event.preventDefault();
    if (!active() || gesture) return;
    const time = performance.now();
    if (time - lastWheel > 250 || getSize() !== appliedSize) wheelSize = getSize();
    const delta = event.deltaY * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? element.clientHeight : 1);
    wheelSize = clampSize(wheelSize * Math.exp(-delta * .002));
    lastWheel = time; queue(wheelSize, [event.clientX, event.clientY]);
  });
  const restoreSelection = () => {
    if (initialTouch && view.state.doc === initialTouch.document && !view.state.selection.eq(initialTouch.selection)) view.dispatch({ selection: initialTouch.selection });
  };
  add('touchstart', event => {
    if (!active()) return;
    if (!initialTouch || event.touches.length === 1) initialTouch = { selection: view.state.selection, document: view.state.doc };
    if (event.touches.length !== 2) return;
    event.preventDefault();
    pinching = true; restoreSelection();
    touch = { ...touchPosition(event.touches), size: getSize() };
  }, true);
  add('touchmove', event => {
    if (!touch || event.touches.length !== 2 || !active()) return;
    event.preventDefault();
    const position = touchPosition(event.touches);
    if (touch.distance > 0) queue(touch.size * position.distance / touch.distance, position.origin);
  });
  const endTouch = event => {
    if (pinching) { event.preventDefault(); restoreSelection(); }
    if (event.touches.length < 2) touch = null;
    if (!event.touches.length) { initialTouch = null; pinching = false; }
  };
  add('touchend', endTouch); add('touchcancel', endTouch);
  // Safari sends GestureEvents for some trackpads instead of ctrl-wheel.
  add('gesturestart', event => {
    if (!active()) return;
    event.preventDefault();
    if (!touch) gesture = { size: getSize() };
  });
  add('gesturechange', event => {
    if (!active()) return;
    event.preventDefault();
    if (gesture && !touch && Number.isFinite(event.scale)) queue(gesture.size * event.scale, Number.isFinite(event.clientX) && Number.isFinite(event.clientY) ? [event.clientX, event.clientY] : null);
  });
  add('gestureend', event => { if (gesture) event.preventDefault(); gesture = null; });
  return () => {
    if (frame !== null) cancelAnimationFrame(frame);
    for (const [type, handler, capture] of listeners) element.removeEventListener(type, handler, capture);
  };
}
