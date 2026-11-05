// Positions use JavaScript / CodeMirror's UTF-16 offsets. Blanking comments
// keeps those offsets unchanged, including for text outside the ASCII range.
function maskNonCode(text) {
  const characters = text.split('');
  const blank = (start, end) => {
    for (let i = start; i < end; i++) if (characters[i] !== '\n' && characters[i] !== '\r') characters[i] = ' ';
  };
  for (let i = 0; i < text.length; i++) {
    if (text[i] === '%') {
      const end = text.indexOf('\n', i);
      blank(i, end < 0 ? text.length : end);
      i = end < 0 ? text.length : end;
    } else if (text[i] === '\\') {
      const begin = /^\\begin\s*\{(verbatim\*?|Verbatim|lstlisting|minted|comment)\}/.exec(text.slice(i, i + 96));
      if (begin) {
        const closing = `\\end{${begin[1]}}`;
        const at = text.indexOf(closing, i + begin[0].length);
        const end = at < 0 ? text.length : at + closing.length;
        blank(i, end); i = end - 1;
        continue;
      }
      const verb = /^\\verb\*?([^A-Za-z\s])/.exec(text.slice(i, i + 16));
      if (verb) {
        const at = text.indexOf(verb[1], i + verb[0].length);
        const newline = text.indexOf('\n', i);
        const end = at >= 0 && (newline < 0 || at < newline) ? at + 1 : (newline < 0 ? text.length : newline);
        blank(i, end); i = end - 1;
        continue;
      }
      if (i + 1 < text.length && !/[A-Za-z@]/.test(text[i + 1])) i++;
    }
  }
  return characters.join('');
}

function escaped(text, offset) {
  let count = 0;
  while (offset > 0 && text[--offset] === '\\') count++;
  return count % 2 === 1;
}

function group(text, start, open = '{', close = '}') {
  if (text[start] !== open) return null;
  let depth = 1;
  for (let i = start + 1; i < text.length; i++) {
    if (text[i] === '\\') { i++; continue; }
    if (text[i] === open) depth++;
    else if (text[i] === close && --depth === 0) return { content: text.slice(start + 1, i), end: i + 1 };
  }
  return null;
}

function lineLookup(text) {
  const starts = [0];
  for (let at = text.indexOf('\n'); at >= 0; at = text.indexOf('\n', at + 1)) starts.push(at + 1);
  return position => {
    let low = 0, high = starts.length;
    while (low + 1 < high) {
      const middle = (low + high) >>> 1;
      if (starts[middle] <= position) low = middle; else high = middle;
    }
    return low + 1;
  };
}

function skipSpace(text, offset) {
  while (/\s/.test(text[offset] || '') && offset < text.length) offset++;
  return offset;
}

function readable(text) {
  return text.replace(/\\(?:textbf|textit|textrm|textsf|texttt|emph|mathrm|mathbf|mathit|text)\s*(?=\{)/g, '')
    .replace(/\\([%&#_$])/g, '$1').replace(/[{}]/g, '').replace(/~/g, ' ').replace(/\s+/g, ' ').trim();
}

function hasParameter(text) {
  for (const match of text.matchAll(/#[1-9]/g)) if (!escaped(text, match.index)) return true;
  return false;
}

function collectOutline(text, lineAt) {
  const levels = { part: 0, chapter: 1, section: 2, subsection: 3, subsubsection: 4, paragraph: 5, subparagraph: 6 };
  const pattern = /\\(part|chapter|section|subsection|subsubsection|paragraph|subparagraph)(\*)?(?![A-Za-z@])/g;
  const result = [];
  for (let match; (match = pattern.exec(text));) {
    if (escaped(text, match.index)) continue;
    let offset = skipSpace(text, pattern.lastIndex);
    if (text[offset] === '[') {
      const optional = group(text, offset, '[', ']');
      if (!optional) continue;
      offset = skipSpace(text, optional.end);
    }
    const title = group(text, offset);
    if (!title) continue;
    if (hasParameter(title.content)) { pattern.lastIndex = title.end; continue; }
    result.push({ level: levels[match[1]], title: readable(title.content), line: lineAt(match.index), from: match.index, to: title.end });
    pattern.lastIndex = title.end;
  }
  return result;
}

function collectLabels(text, lineAt, outline, path) {
  const pattern = /\\label(?![A-Za-z@])\s*\{/g, result = [];
  let heading = 0;
  for (let match; (match = pattern.exec(text));) {
    if (escaped(text, match.index)) continue;
    const value = group(text, pattern.lastIndex - 1);
    if (!value) continue;
    while (heading + 1 < outline.length && outline[heading + 1].from < match.index) heading++;
    const context = outline[heading]?.from < match.index ? outline[heading].title : '';
    const key = value.content.trim();
    if (key && !hasParameter(key)) result.push({ key, path, line: lineAt(match.index), from: match.index, context });
    pattern.lastIndex = value.end;
  }
  return result;
}

export function extractTexMetadata(content, path = '') {
  const text = maskNonCode(content), lineAt = lineLookup(content);
  const outline = collectOutline(text, lineAt);
  return { outline, labels: collectLabels(text, lineAt, outline, path) };
}

export function extractOutline(content) {
  return collectOutline(maskNonCode(content), lineLookup(content));
}

export function extractLabels(content, path = '') {
  return extractTexMetadata(content, path).labels;
}

function bibValue(text, offset) {
  offset = skipSpace(text, offset);
  if (text[offset] === '{') return group(text, offset);
  if (text[offset] === '"') {
    for (let end = offset + 1; end < text.length; end++) {
      if (text[end] === '\\') { end++; continue; }
      if (text[end] === '"') return { content: text.slice(offset + 1, end), end: end + 1 };
    }
    return null;
  }
  const match = /^[^,#\s}\)]+/.exec(text.slice(offset));
  return match ? { content: match[0], end: offset + match[0].length } : null;
}

function bibFields(text) {
  const result = {}, pattern = /(?:^|,)\s*([A-Za-z][\w-]*)\s*=/g;
  for (let match; (match = pattern.exec(text));) {
    let value = bibValue(text, pattern.lastIndex);
    if (!value) continue;
    let content = value.content, end = value.end;
    for (;;) {
      const next = skipSpace(text, end);
      if (text[next] !== '#') break;
      value = bibValue(text, next + 1);
      if (!value) break;
      content += value.content; end = value.end;
    }
    result[match[1].toLowerCase()] = readable(content);
    pattern.lastIndex = end;
  }
  return result;
}

function bibEntry(text, start) {
  if (text[start] === '{') return group(text, start);
  let braces = 0, quoted = false;
  for (let i = start + 1; i < text.length; i++) {
    if (text[i] === '\\') { i++; continue; }
    if (text[i] === '{') braces++;
    else if (text[i] === '}') braces--;
    else if (text[i] === '"' && braces === 0) quoted = !quoted;
    else if (text[i] === ')' && braces === 0 && !quoted) return { content: text.slice(start + 1, i), end: i + 1 };
  }
  return null;
}

export function extractCitations(content, path = '') {
  const lineAt = lineLookup(content), pattern = /@([A-Za-z]+)\s*([{(])/g, result = [];
  for (let match; (match = pattern.exec(content));) {
    const entry = bibEntry(content, pattern.lastIndex - 1);
    if (!entry) continue;
    pattern.lastIndex = entry.end;
    if (/^(comment|string|preamble)$/i.test(match[1])) continue;
    const key = /^\s*([^\s,{}()]+)\s*,/.exec(entry.content);
    if (!key) continue;
    const fields = bibFields(entry.content.slice(key[0].length));
    result.push({ key: key[1], title: fields.title || '', author: fields.author || fields.editor || '', year: fields.year || fields.date || '', path, line: lineAt(match.index) });
  }
  return result;
}

export function texCompletionSource(getItems) {
  return context => {
    const start = Math.max(0, context.pos - 8192);
    const before = maskNonCode(context.state.doc.sliceString(start, context.pos));
    const match = /\\([cC]ite[A-Za-z]*\*?|(?:paren|text|auto|foot|smart|super)cite[A-Za-z]*\*?|nocite|(?:ref|eqref|autoref|[cC]ref|pageref|vref|nameref)\*?)(?:\s*\[[^\]]*\])*\s*\{([^{}]*)$/.exec(before);
    if (!match || escaped(before, match.index)) return null;
    const list = match[2], comma = list.lastIndexOf(',');
    const part = list.slice(comma + 1), leading = /^\s*/.exec(part)[0].length;
    const typed = part.slice(leading);
    if (/\s/.test(typed)) return null;
    const from = context.pos - typed.length;
    const items = getItems() || {}, citation = /cite/i.test(match[1]);
    const entries = citation ? items.citations || [] : items.labels || [];
    const seen = new Set(), used = new Set(list.slice(0, comma + 1).split(',').map(key => key.trim()));
    const options = [];
    for (const item of entries) {
      if (!item?.key || seen.has(item.key) || used.has(item.key)) continue;
      seen.add(item.key);
      const detail = citation ? [item.author, item.year].filter(Boolean).join(' · ') : item.context || item.path;
      const info = [citation ? item.title : item.context, item.path && `${item.path}:${item.line || 1}`].filter(Boolean).join('\n');
      options.push({ label: item.key, type: citation ? 'text' : 'variable', detail, info, apply: item.key });
    }
    return options.length ? { from, options, validFor: /^[^{}\s,]*$/ } : null;
  };
}

function normalPath(path) {
  const parts = [], value = path.replace(/\\/g, '/');
  for (const part of value.split('/')) {
    if (!part || part === '.') continue;
    if (part === '..' && parts.length && parts.at(-1) !== '..') parts.pop();
    else parts.push(part);
  }
  return (value.startsWith('/') ? '/' : '') + parts.join('/');
}

function relativeLogPath(path, root) {
  const value = path.replace(/\\/g, '/').replace(/^"|"$/g, '');
  const absolute = /^(?:[A-Za-z]:\/|\/)/.test(value);
  if (!root) {
    const relative = normalPath(value);
    return absolute || relative === '..' || relative.startsWith('../') ? null : relative;
  }
  const base = normalPath(root).replace(/\/$/, '');
  const complete = normalPath(absolute ? value : `${base}/${value}`);
  const windows = /^[A-Za-z]:[\\/]/.test(root) || /^[\\/]{2}/.test(root);
  const prefix = `${base}/`;
  const inside = windows ? complete.toLowerCase().startsWith(prefix.toLowerCase()) : complete.startsWith(prefix);
  return inside ? complete.slice(prefix.length) : null;
}

export function extractLogLocations(log, files = [], projectRoot = '') {
  const known = files.map(file => typeof file === 'string' ? file : file.path).filter(Boolean)
    .map(path => ({ path, normalized: normalPath(path) }))
    .sort((a, b) => b.normalized.length - a.normalized.length);
  const result = [], seen = new Set();
  for (const row of log.replace(/\x1b\[[0-9;]*m/g, '').split(/\r?\n/)) {
    const match = /^\s*"?(.+?\.(?:tex|bib|sty|cls|ltx|cfg))"?:(\d+):\s*(.*)$/i.exec(row);
    const bib = match ? null : /^(.*?)---line\s+(\d+)\s+of file\s+(.+?)\s*$/i.exec(row);
    if (!match && !bib) continue;
    const candidate = relativeLogPath(match ? match[1] : bib[3], projectRoot);
    if (!candidate) continue;
    const found = known.find(item => item.normalized === candidate) ||
      (/^(?:[A-Za-z]:[\\/]|[\\/]{2})/.test(projectRoot) ? known.find(item => item.normalized.toLowerCase() === candidate.toLowerCase()) : null);
    const line = Number((match || bib)[2]);
    if (!found || !Number.isSafeInteger(line) || line < 1) continue;
    const message = (match ? match[3] : bib[1]).trim(), key = `${found.path}:${line}:${message}`;
    if (!seen.has(key)) { seen.add(key); result.push({ path: found.path, line, message }); }
    if (result.length >= 100) break;
  }
  return result;
}
