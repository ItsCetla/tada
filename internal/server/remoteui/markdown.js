// Small, safe Markdown renderer. It builds DOM nodes directly and only ever
// assigns text through textContent, so model output can never inject HTML.
// Supported: fenced code, inline code, headings, bold, italics, lists,
// block quotes, tables, rules, links and bare http(s) URLs.

export function safeHref(raw) {
  try {
    const url = new URL(raw, window.location.href);
    if (url.protocol === 'http:' || url.protocol === 'https:') return url.href;
  } catch {
    // Not a URL.
  }
  return null;
}

function el(tag, cls) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  return node;
}

function link(parent, text, href) {
  const safe = safeHref(href);
  if (!safe) {
    parent.append(text);
    return;
  }
  const a = el('a');
  a.href = safe;
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  a.textContent = text;
  parent.append(a);
}

const INLINE = new RegExp(
  [
    '(`+)([\\s\\S]*?[^`])\\1(?!`)', // 1,2 code
    '\\*\\*(?=\\S)([\\s\\S]*?\\S)\\*\\*', // 3 bold
    '__(?=\\S)([\\s\\S]*?\\S)__', // 4 bold
    '\\[([^\\]\\n]+)\\]\\(\\s*<?([^)\\s>]+)>?(?:\\s+"[^"]*")?\\s*\\)', // 5,6 link
    '(https?:\\/\\/[^\\s<>()]*[^\\s<>().,;:!?\'"\\]])', // 7 bare url
    '\\*(?=[^\\s*])([^*\\n]*?[^\\s*])\\*', // 8 em
  ].join('|'),
  'g',
);

export function renderInline(text, parent) {
  let last = 0;
  INLINE.lastIndex = 0;
  let m;
  while ((m = INLINE.exec(text)) !== null) {
    if (m.index > last) parent.append(text.slice(last, m.index));
    // The nested calls below reuse INLINE and reset its position, so take
    // the end of this match now and put it back afterwards.
    const end = m.index + m[0].length;
    if (m[2] !== undefined) {
      const code = el('code');
      code.textContent = m[2].replace(/^ (.*) $/s, '$1');
      parent.append(code);
    } else if (m[3] !== undefined || m[4] !== undefined) {
      const b = el('strong');
      renderInline(m[3] ?? m[4], b);
      parent.append(b);
    } else if (m[5] !== undefined) {
      link(parent, m[5], m[6]);
    } else if (m[7] !== undefined) {
      link(parent, m[7], m[7]);
    } else if (m[8] !== undefined) {
      const i = el('em');
      renderInline(m[8], i);
      parent.append(i);
    }
    INLINE.lastIndex = end;
    last = end;
  }
  if (last < text.length) parent.append(text.slice(last));
}

function renderLines(lines, parent) {
  lines.forEach((line, i) => {
    if (i > 0) parent.append(el('br'));
    renderInline(line, parent);
  });
}

const FENCE = /^\s{0,3}(`{3,}|~{3,})\s*([^`\s]*)/;
const HEADING = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const LIST = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const QUOTE = /^\s{0,3}>\s?(.*)$/;
const RULE = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const TABLE_SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;

function isBlockStart(line, next) {
  return (
    FENCE.test(line) ||
    HEADING.test(line) ||
    LIST.test(line) ||
    QUOTE.test(line) ||
    RULE.test(line) ||
    (line.includes('|') && next !== undefined && TABLE_SEP.test(next))
  );
}

function splitRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  return s
    .replace(/\\\|/g, '\u0000')
    .split('|')
    .map((c) => c.trim().replace(/\u0000/g, '|'));
}

export function renderMarkdown(src, parent) {
  const lines = String(src).replace(/\r\n?/g, '\n').split('\n');
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];

    const fence = line.match(FENCE);
    if (fence) {
      const marker = fence[1];
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(marker)) {
        body.push(lines[i]);
        i++;
      }
      i++;
      const pre = el('pre', 'md-code');
      if (fence[2]) pre.dataset.lang = fence[2];
      const code = el('code');
      code.textContent = body.join('\n');
      pre.append(code);
      parent.append(pre);
      continue;
    }

    if (!line.trim()) {
      i++;
      continue;
    }

    const heading = line.match(HEADING);
    if (heading) {
      const h = el('p', 'md-h md-h' + heading[1].length);
      renderInline(heading[2], h);
      parent.append(h);
      i++;
      continue;
    }

    if (RULE.test(line)) {
      parent.append(el('hr'));
      i++;
      continue;
    }

    if (line.includes('|') && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1])) {
      const wrap = el('div', 'md-table');
      const table = el('table');
      const head = el('tr');
      for (const cell of splitRow(line)) {
        const th = el('th');
        renderInline(cell, th);
        head.append(th);
      }
      const thead = el('thead');
      thead.append(head);
      table.append(thead);
      const tbody = el('tbody');
      i += 2;
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
        const tr = el('tr');
        for (const cell of splitRow(lines[i])) {
          const td = el('td');
          renderInline(cell, td);
          tr.append(td);
        }
        tbody.append(tr);
        i++;
      }
      table.append(tbody);
      wrap.append(table);
      parent.append(wrap);
      continue;
    }

    if (QUOTE.test(line)) {
      const body = [];
      while (i < lines.length && QUOTE.test(lines[i])) {
        body.push(lines[i].match(QUOTE)[1]);
        i++;
      }
      const bq = el('blockquote');
      renderMarkdown(body.join('\n'), bq);
      parent.append(bq);
      continue;
    }

    const item = line.match(LIST);
    if (item) {
      i = renderList(lines, i, parent);
      continue;
    }

    const para = [];
    while (i < lines.length && lines[i].trim()) {
      if (para.length && isBlockStart(lines[i], lines[i + 1])) break;
      para.push(lines[i].trim());
      i++;
    }
    const p = el('p');
    renderLines(para, p);
    parent.append(p);
  }
}

function renderList(lines, start, parent) {
  const first = lines[start].match(LIST);
  const indent = first[1].length;
  const ordered = /\d/.test(first[2]);
  const list = el(ordered ? 'ol' : 'ul');
  if (ordered) {
    const n = parseInt(first[2], 10);
    if (n !== 1) list.start = n;
  }
  let i = start;
  while (i < lines.length) {
    const m = lines[i].match(LIST);
    if (!m || m[1].length < indent) break;
    if (m[1].length > indent) {
      // Nested list belongs to the previous item.
      const host = list.lastElementChild || list;
      i = renderList(lines, i, host);
      continue;
    }
    if (/\d/.test(m[2]) !== ordered) break;
    const li = el('li');
    const body = [m[3]];
    i++;
    // Continuation lines: indented, non-list, non-blank.
    while (
      i < lines.length &&
      lines[i].trim() &&
      !LIST.test(lines[i]) &&
      /^\s+/.test(lines[i]) &&
      !FENCE.test(lines[i])
    ) {
      body.push(lines[i].trim());
      i++;
    }
    renderLines(body, li);
    list.append(li);
    // Allow a single blank line between items of a loose list.
    if (
      i + 1 < lines.length &&
      !lines[i]?.trim() &&
      LIST.test(lines[i + 1]) &&
      lines[i + 1].match(LIST)[1].length >= indent
    ) {
      i++;
    }
  }
  parent.append(list);
  return i;
}
