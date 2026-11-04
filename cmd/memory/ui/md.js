// Small safe Markdown renderer for handoff text. It builds DOM nodes with createElement and
// text nodes only, so raw HTML in the input is shown as literal text and can never run.
// Supported: # headings, paragraphs, - * + and 1. lists (nested by indent), > quotes, fenced
// code, tables, horizontal rules, **bold**, *italic*, `code`, and [text](url) where url is
// http, https or mailto. Any other link target is shown as plain text.
"use strict";

(function () {
  function el(tag, text) {
    const e = document.createElement(tag);
    if (text != null) e.textContent = text;
    return e;
  }

  function safeUrl(u) {
    return /^(https?:\/\/|mailto:)[^\s<>"']+$/i.test(u.trim()) ? u.trim() : null;
  }

  // Inline spans: code first (no nesting inside), then links, bold, italic.
  function inline(parent, s) {
    const re = /`([^`]+)`|\[([^\]]+)\]\(([^)\s]+)\)|\*\*([^*]+)\*\*|(^|[^*\w])\*([^*\s][^*]*)\*(?!\w)/g;
    let last = 0, m;
    while ((m = re.exec(s)) !== null) {
      if (m[5] !== undefined) { // italic: the lead char belongs to the preceding text
        parent.append(document.createTextNode(s.slice(last, m.index) + m[5]));
      } else {
        parent.append(document.createTextNode(s.slice(last, m.index)));
      }
      if (m[1] !== undefined) parent.append(el("code", m[1]));
      else if (m[2] !== undefined) {
        const url = safeUrl(m[3]);
        if (url) {
          const a = el("a");
          a.href = url;
          a.rel = "noreferrer noopener";
          a.target = "_blank";
          inline(a, m[2]);
          parent.append(a);
        } else parent.append(document.createTextNode(m[0]));
      } else if (m[4] !== undefined) {
        const b = el("strong");
        inline(b, m[4]);
        parent.append(b);
      } else {
        const i = el("em");
        inline(i, m[6]);
        parent.append(i);
      }
      last = re.lastIndex;
    }
    parent.append(document.createTextNode(s.slice(last)));
  }

  const BULLET = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;

  function cells(line) {
    return line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
  }

  function renderMarkdown(src) {
    const root = el("div");
    root.className = "md";
    const lines = String(src == null ? "" : src).replace(/\r\n?/g, "\n").split("\n");
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (!line.trim()) { i++; continue; }

      const fence = /^\s*(```|~~~)/.exec(line);
      if (fence) {
        const buf = [];
        i++;
        while (i < lines.length && !lines[i].trim().startsWith(fence[1])) buf.push(lines[i++]);
        i++;
        const pre = el("pre");
        pre.append(el("code", buf.join("\n")));
        root.append(pre);
        continue;
      }

      const hd = /^(#{1,6})\s+(.*?)\s*#*\s*$/.exec(line);
      if (hd) {
        const h = el("h" + Math.min(6, hd[1].length + 2)); // page title is h1, card title h2
        inline(h, hd[2]);
        root.append(h);
        i++;
        continue;
      }

      if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { root.append(el("hr")); i++; continue; }

      if (/^\s*>/.test(line)) {
        const buf = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) buf.push(lines[i++].replace(/^\s*>\s?/, ""));
        const q = el("blockquote");
        const p = el("p");
        inline(p, buf.join(" "));
        q.append(p);
        root.append(q);
        continue;
      }

      if (line.includes("|") && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(lines[i + 1])) {
        const head = cells(line);
        i += 2;
        const table = el("table"), thead = el("thead"), tr = el("tr"), tbody = el("tbody");
        for (const c of head) { const th = el("th"); th.scope = "col"; inline(th, c); tr.append(th); }
        thead.append(tr);
        while (i < lines.length && lines[i].includes("|") && lines[i].trim()) {
          const r = el("tr");
          for (const c of cells(lines[i++])) { const td = el("td"); inline(td, c); r.append(td); }
          tbody.append(r);
        }
        table.append(thead, tbody);
        const wrap = el("div");
        wrap.className = "tablewrap";
        wrap.append(table);
        root.append(wrap);
        continue;
      }

      if (BULLET.test(line)) {
        // Nested lists by indent: a stack of [indent, list element].
        const stack = [];
        while (i < lines.length && BULLET.test(lines[i])) {
          const m = BULLET.exec(lines[i]);
          const indent = m[1].replace(/\t/g, "    ").length;
          const ordered = /\d/.test(m[2]);
          while (stack.length && indent < stack[stack.length - 1].indent) stack.pop();
          let top = stack[stack.length - 1];
          if (!top || indent > top.indent) {
            const list = el(ordered ? "ol" : "ul");
            if (top && top.list.lastElementChild) top.list.lastElementChild.append(list);
            else root.append(list);
            top = { indent, list };
            stack.push(top);
          }
          const li = el("li");
          inline(li, m[3]);
          top.list.append(li);
          i++;
        }
        continue;
      }

      // Paragraph: consecutive plain lines.
      const buf = [];
      while (i < lines.length && lines[i].trim() && !/^\s*(```|~~~|#{1,6}\s|>)/.test(lines[i]) && !BULLET.test(lines[i])) buf.push(lines[i++].trim());
      if (!buf.length) { buf.push(lines[i++].trim()); }
      const p = el("p");
      inline(p, buf.join(" "));
      root.append(p);
    }
    return root;
  }

  window.renderMarkdown = renderMarkdown;
})();
