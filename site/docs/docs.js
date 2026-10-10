// DokWalt docs: theme switch, copy buttons, heading anchors, "On this page" tracking,
// the mobile menu and ⌘K search over search.json. Vanilla JS, no dependencies.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;
const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

// ───────── Theme switch (same choice and storage as the landing page) ─────────
(function themeSwitch() {
  const root = document.documentElement, btn = $("#theme-toggle");
  const meta = $('meta[name="theme-color"]');
  const system = matchMedia("(prefers-color-scheme: light)");
  const saved = () => { try { return localStorage.getItem("theme"); } catch { return null; } };
  function apply(theme) {
    root.dataset.theme = theme;
    meta.content = theme === "light" ? "#f7f7fb" : "#07070c";
    const next = theme === "light" ? "dark" : "light";
    btn.setAttribute("aria-label", `Switch to ${next} theme`);
    btn.title = `Switch to ${next} theme`;
  }
  apply(root.dataset.theme === "light" ? "light" : "dark");
  btn.addEventListener("click", () => {
    const theme = root.dataset.theme === "light" ? "dark" : "light";
    try { localStorage.setItem("theme", theme); } catch {}
    if (!document.startViewTransition || reduced) return apply(theme);
    const r = btn.getBoundingClientRect();
    const x = r.left + r.width / 2, y = r.top + r.height / 2;
    const radius = Math.hypot(Math.max(x, innerWidth - x), Math.max(y, innerHeight - y));
    document.startViewTransition(() => apply(theme)).ready.then(() => {
      root.animate(
        { clipPath: [`circle(0px at ${x}px ${y}px)`, `circle(${radius}px at ${x}px ${y}px)`] },
        { duration: 600, easing: "cubic-bezier(.22, 1, .36, 1)", pseudoElement: "::view-transition-new(root)" },
      );
    }).catch(() => {});
  });
  system.addEventListener("change", (e) => { if (!saved()) apply(e.matches ? "light" : "dark"); });
})();

if (!isMac) $$(".kbd-k").forEach((k) => (k.textContent = "Ctrl K"));

// ───────── Code: copy buttons ─────────
$$(".code .copy").forEach((btn) => btn.addEventListener("click", async () => {
  const text = $("pre", btn.closest(".code")).innerText.replace(/\n$/, "");
  try { await navigator.clipboard.writeText(text); } catch { return; }
  btn.classList.add("copied");
  setTimeout(() => btn.classList.remove("copied"), 1400);
}));

// ───────── Headings: anchors and "On this page" ─────────
const headings = $$(".prose :is(h2, h3, h4)[id]");
headings.forEach((h) => {
  const a = Object.assign(document.createElement("a"), { className: "anchor", href: "#" + h.id, textContent: "#" });
  a.setAttribute("aria-label", "Link to this section");
  h.prepend(a);
});

(function tocSpy() {
  const links = new Map($$(".docs-toc a").map((a) => [decodeURIComponent(a.hash.slice(1)), a]));
  if (!links.size) return;
  const tracked = headings.filter((h) => links.has(h.id));
  let current = null, queued = false;
  function update() {
    queued = false;
    let active = tracked[0];
    for (const h of tracked) {
      if (h.getBoundingClientRect().top < 130) active = h; else break;
    }
    // At the very bottom, the last section is the one being read.
    if (innerHeight + scrollY >= document.documentElement.scrollHeight - 4) active = tracked[tracked.length - 1];
    if (active === current) return;
    if (current) links.get(current.id).classList.remove("active");
    current = active;
    const link = links.get(active.id);
    link.classList.add("active");
    const toc = link.closest(".docs-toc"), lr = link.getBoundingClientRect(), tr = toc.getBoundingClientRect();
    if (lr.top < tr.top + 40 || lr.bottom > tr.bottom - 40) toc.scrollTop += lr.top - tr.top - tr.height / 2;
  }
  addEventListener("scroll", () => { if (!queued) { queued = true; requestAnimationFrame(update); } }, { passive: true });
  update();
})();

// ───────── Sidebar: keep the current page in view; drawer on small screens ─────────
(function sidebar() {
  const side = $("#docs-side"), toggle = $(".side-toggle");
  if (!side) return;
  const active = $(".side-nav a.active", side);
  if (active) {
    const r = active.getBoundingClientRect(), sr = side.getBoundingClientRect();
    if (r.bottom > sr.bottom) side.scrollTop = r.top - sr.top - sr.height / 3;
  }
  if (!toggle) return;
  const set = (open) => {
    document.body.classList.toggle("side-open", open);
    toggle.setAttribute("aria-expanded", String(open));
  };
  toggle.addEventListener("click", (e) => { e.stopPropagation(); set(!document.body.classList.contains("side-open")); });
  document.addEventListener("click", (e) => {
    if (document.body.classList.contains("side-open") && (!side.contains(e.target) || e.target.closest("a"))) set(false);
  });
  addEventListener("keydown", (e) => { if (e.key === "Escape") set(false); });
})();

// ───────── Search ─────────
(function search() {
  const box = $("#search"), input = $("#search-input"), list = $("#search-results");
  let index = null, loading = null, results = [], sel = 0, opener = null;

  const load = () => loading || (loading = fetch("/docs/search.json")
    .then((r) => r.json())
    .then((d) => (index = d.map((e) => ({ ...e, lt: e.t.toLowerCase(), lh: (e.h || "").toLowerCase(), lx: e.x.toLowerCase() }))))
    .catch(() => (index = [])));

  const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const reEsc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

  function mark(text, words) {
    if (!words.length) return esc(text);
    const re = new RegExp(`(${words.map(reEsc).join("|")})`, "gi");
    return text.split(re).map((part, i) => (i % 2 ? `<mark>${esc(part)}</mark>` : esc(part))).join("");
  }

  function snippet(e, words) {
    const at = Math.min(...words.map((w) => e.lx.indexOf(w)).filter((i) => i >= 0), Infinity);
    if (at === Infinity) return e.x.slice(0, 160);
    const start = Math.max(0, at - 50);
    return (start > 0 ? "…" : "") + e.x.slice(start, start + 180);
  }

  function find(q) {
    const words = q.toLowerCase().split(/\s+/).filter(Boolean);
    if (!words.length) return { words, items: index.filter((e) => !e.h).map((e) => ({ e })) };
    const phrase = words.join(" ");
    const items = [];
    for (const e of index) {
      let score = 0, ok = true;
      for (const w of words) {
        const inH = e.lh.includes(w), inT = e.lt.includes(w), inX = e.lx.includes(w);
        if (!inH && !inT && !inX) { ok = false; break; }
        score += (inH ? 12 : 0) + (inT ? 6 : 0) + (inX ? 1 : 0);
      }
      if (!ok) continue;
      if (e.lh.startsWith(phrase) || (!e.h && e.lt.startsWith(phrase))) score += 20;
      if (!e.h) score += 2; // the topic itself before its sections, on equal terms
      items.push({ e, score });
    }
    items.sort((a, b) => b.score - a.score);
    return { words, items: items.slice(0, 14) };
  }

  function render() {
    if (!index) { list.innerHTML = `<li class="search-empty">Loading…</li>`; return; }
    const { words, items } = find(input.value.trim());
    results = items.map(({ e }) => e);
    sel = 0;
    if (!results.length) {
      list.innerHTML = `<li class="search-empty">No results for “${esc(input.value.trim())}”</li>`;
      return;
    }
    const label = words.length ? "" : `<li class="search-label" role="presentation">All topics</li>`;
    list.innerHTML = label + results.map((e, i) => {
      const href = `/docs/${e.s}/${e.a ? "#" + e.a : ""}`;
      const path = e.h ? `${esc(e.t)} ›` : "Topic";
      const title = mark(e.h || e.t, words);
      const snip = words.length ? mark(snippet(e, words), words) : esc(e.x.slice(0, 150));
      return `<li role="option" data-i="${i}"${i === 0 ? ' class="sel" aria-selected="true"' : ""}><a href="${href}">
        <span class="r-path">${path}</span><span class="r-title">${title}</span><span class="r-snip">${snip}</span></a></li>`;
    }).join("");
  }

  function select(i) {
    const items = $$("li[role=option]", list);
    if (!items.length) return;
    sel = (i + items.length) % items.length;
    items.forEach((li, j) => { li.classList.toggle("sel", j === sel); li.setAttribute("aria-selected", String(j === sel)); });
    items[sel].scrollIntoView({ block: "nearest" });
  }

  function open() {
    if (!box.hidden) return;
    opener = document.activeElement;
    box.hidden = false;
    document.documentElement.style.overflow = "hidden";
    input.select();
    input.focus();
    render();
    load().then(render);
  }
  function close() {
    if (box.hidden) return;
    box.hidden = true;
    document.documentElement.style.overflow = "";
    if (opener && opener.focus) opener.focus();
  }

  $$("[data-search]").forEach((b) => b.addEventListener("click", open));
  $$("[data-close]", box).forEach((b) => b.addEventListener("click", close));
  input.addEventListener("input", render);
  list.addEventListener("mousemove", (e) => {
    const li = e.target.closest("li[role=option]");
    if (li && +li.dataset.i !== sel) select(+li.dataset.i);
  });
  list.addEventListener("click", (e) => { if (e.target.closest("a")) close(); });

  addEventListener("keydown", (e) => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
    if ((e.key === "k" && (e.metaKey || e.ctrlKey)) || (e.key === "/" && !typing && box.hidden)) {
      e.preventDefault();
      box.hidden ? open() : close();
      return;
    }
    if (box.hidden) return;
    if (e.key === "Escape") { e.preventDefault(); close(); }
    else if (e.key === "ArrowDown") { e.preventDefault(); select(sel + 1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); select(sel - 1); }
    else if (e.key === "Enter") {
      const a = $(`li[data-i="${sel}"] a`, list);
      if (a) { e.preventDefault(); close(); location.href = a.href; }
    }
  });
})();
