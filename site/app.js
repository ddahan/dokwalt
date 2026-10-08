// DokWalt landing page. Vanilla JS, no dependencies.
// Every animation loop waits until its element is on screen, so off-screen
// sections cost nothing.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const rand = (a, b) => a + Math.random() * (b - a);
const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;

// ───────── Visibility ─────────
const visible = new WeakMap();
const waiters = new WeakMap();
const visIO = new IntersectionObserver((entries) => {
  for (const e of entries) {
    visible.set(e.target, e.isIntersecting);
    if (e.isIntersecting) {
      (waiters.get(e.target) || []).forEach((r) => r());
      waiters.set(e.target, []);
    }
  }
}, { threshold: 0.15 });
function track(el) { visIO.observe(el); }
function whenVisible(el) {
  if (visible.get(el)) return Promise.resolve();
  return new Promise((r) => waiters.set(el, [...(waiters.get(el) || []), r]));
}
// Sleep, then wait until the element is visible again.
async function pause(el, ms) { await sleep(ms); await whenVisible(el); }

// ───────── Nav, title words, reveal ─────────
const nav = $("#nav");
const onScroll = () => nav.classList.toggle("scrolled", scrollY > 8);
addEventListener("scroll", onScroll, { passive: true });
onScroll();

$$(".hero-title .word").forEach((w, i) => w.style.setProperty("--i", i));

function sliceTitleGradient() {
  const words = $$(".hero-title .word.gradient-text");
  if (!words.length) return;
  const left = Math.min(...words.map((w) => w.offsetLeft));
  const right = Math.max(...words.map((w) => w.offsetLeft + w.offsetWidth));
  for (const w of words) {
    w.style.setProperty("--lw", `${right - left}px`);
    w.style.setProperty("--bx", `${left - w.offsetLeft}px`);
  }
}
document.fonts.ready.then(sliceTitleGradient);
addEventListener("resize", sliceTitleGradient);

const revealIO = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (!e.isIntersecting) continue;
    e.target.classList.add("in");
    revealIO.unobserve(e.target);
    if (e.target.classList.contains("stat")) countUp($(".count", e.target));
  }
}, { threshold: 0.12, rootMargin: "0px 0px -6% 0px" });
$$(".reveal").forEach((el) => revealIO.observe(el));

function countUp(el) {
  const from = +(el.dataset.from || 0), to = +el.dataset.to;
  if (reduced) { el.textContent = to; return; }
  const t0 = performance.now(), dur = 1400;
  const tick = (t) => {
    const p = Math.min(1, (t - t0) / dur), e = 1 - Math.pow(1 - p, 4);
    el.textContent = Math.round(from + (to - from) * e);
    if (p < 1) requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

// ───────── Copy buttons ─────────
$$(".copy").forEach((btn) => btn.addEventListener("click", async () => {
  const text = $(btn.dataset.copy).textContent.trim();
  try { await navigator.clipboard.writeText(text); } catch {
    const ta = Object.assign(document.createElement("textarea"), { value: text });
    document.body.append(ta); ta.select(); document.execCommand("copy"); ta.remove();
  }
  btn.classList.add("copied");
  setTimeout(() => btn.classList.remove("copied"), 1600);
}));

// ───────── Spotlight cards & hero tilt ─────────
$$(".spot").forEach((card) => card.addEventListener("pointermove", (e) => {
  const r = card.getBoundingClientRect();
  card.style.setProperty("--mx", `${e.clientX - r.left}px`);
  card.style.setProperty("--my", `${e.clientY - r.top}px`);
}));

const tilt = $("#hero-tilt");
if (!reduced && matchMedia("(hover: hover)").matches) {
  const hero = $(".hero");
  hero.addEventListener("pointermove", (e) => {
    const r = tilt.getBoundingClientRect();
    const x = (e.clientX - (r.left + r.width / 2)) / innerWidth;
    const y = (e.clientY - (r.top + r.height / 2)) / innerHeight;
    tilt.style.transform = `rotateY(${x * 10}deg) rotateX(${-y * 8}deg)`;
  });
  hero.addEventListener("pointerleave", () => { tilt.style.transform = ""; });
}

// ───────── Terminal helpers ─────────
const SPIN = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏";
const ok = '<span class="c-ok">✓</span>';
const muted = (s) => `<span class="c-mute">${s}</span>`;
const faint = (s) => `<span class="c-faint">${s}</span>`;
const title = (s, rest = "") => `<span class="c-acc">◆ ${s}</span>${rest ? "  " + rest : ""}`;

function line(term, html = "") {
  const el = document.createElement("span");
  el.className = "ln";
  el.innerHTML = html;
  term.append(el);
  return el;
}

async function typeCmd(term, cmd, token, prompt = muted("$")) {
  const el = line(term, `${prompt} `);
  const text = document.createElement("span");
  const cur = Object.assign(document.createElement("span"), { className: "cursor" });
  el.append(text, cur);
  for (const ch of cmd) {
    if (token && token.dead) return el;
    text.textContent += ch;
    await sleep(reduced ? 0 : rand(28, 70));
  }
  await sleep(reduced ? 0 : 280);
  cur.remove();
  return el;
}

// Spinner line that resolves into a check mark.
async function stepLine(term, text, ms, dur = "", token) {
  const el = line(term);
  if (reduced || ms < 120) { el.innerHTML = `${ok} ${text}${dur ? " " + faint(dur) : ""}`; await sleep(reduced ? 0 : ms); return el; }
  let i = 0;
  const t0 = performance.now();
  while (performance.now() - t0 < ms) {
    if (token && token.dead) break;
    el.innerHTML = `<span class="spin">${SPIN[i++ % SPIN.length]}</span> ${text}`;
    await sleep(80);
  }
  el.innerHTML = `${ok} ${text}${dur ? " " + faint(dur) : ""}`;
  return el;
}

// ───────── Hero deploy terminal ─────────
(async function heroTerminal() {
  const term = $("#hero-term");
  const card = term.closest(".term");
  const fc1 = $(".fc-1"), fc2 = $(".fc-2");
  track(card);
  await sleep(700);
  for (;;) {
    await whenVisible(card);
    term.innerHTML = "";
    fc1.classList.remove("show"); fc2.classList.remove("show");
    await typeCmd(term, 'dokwalt deploy -m "Faster checkout"');
    await sleep(350);
    line(term, title("Deploying shop", "to prod (linux/arm64)"));
    await sleep(300);
    await stepLine(term, "Built migrate, web", 1400, "1.4s");
    await stepLine(term, "Uploaded 16.6 MiB (25.4 MiB/s)", 900, "652ms");
    await stepLine(term, "Services — cache: keeps its data (named volume cachedata), migrate: runs once per deploy, web: zero-downtime swap", 380);
    await stepLine(term, "Created release v3", 260);
    await stepLine(term, "Stateful services unchanged", 200);
    await stepLine(term, "Containers running", 1300, "3.8s");
    await stepLine(term, "web is healthy", 900);
    await stepLine(term, "v3 is live", 250);
    fc1.classList.add("show");
    await stepLine(term, "Previous version stops in 10s (in-flight requests finish)", 400);
    line(term, `${ok} shop is live at <span class="link-u">https://shop.example.test</span>`);
    await sleep(500);
    fc2.classList.add("show");
    const prompt = line(term, `${muted("$")} `);
    prompt.append(Object.assign(document.createElement("span"), { className: "cursor" }));
    await pause(card, 7000);
  }
})();

// ───────── Blue/green flow diagram ─────────
(function flowDiagram() {
  const svg = $("#flow");
  const card = svg.closest(".flow-card");
  const layer = $("#packets");
  const statusEl = $("#flow-status");
  const replay = $("#flow-replay");
  const steps = $$("#deploy-steps li");
  const paths = Object.fromEntries(["p-in", "p-b", "p-g", "p-bdb", "p-gdb"].map((id) => {
    const p = $("#" + id);
    return [id, { p, len: p.getTotalLength() }];
  }));
  const slots = { blue: $("#slot-blue"), green: $("#slot-green") };
  const color = { blue: "#60A5FA", green: "#34D399" };
  const other = (c) => (c === "blue" ? "green" : "blue");
  track(card);

  const state = { live: "blue", ver: { blue: 2, green: 3 }, target: "blue" };
  const packets = [];

  function setSlot(c, cls, ver) {
    const s = slots[c];
    s.classList.remove("idle", "live", "checking", "draining");
    if (cls) cls.split(" ").forEach((k) => s.classList.add(k));
    if (ver != null) $(".slot-ver", s).textContent = "v" + ver;
    const on = !s.classList.contains("idle");
    $("#p-" + c[0]).classList.toggle("off", !on);
    $("#p-" + c[0] + "db").classList.toggle("off", !on);
  }
  function status(text, kind = "ok") {
    statusEl.textContent = text;
    statusEl.style.color = kind === "ok" ? "var(--green)" : "var(--blue)";
  }
  function stepTo(i) {
    steps.forEach((li, j) => {
      li.classList.toggle("active", j === i);
      li.classList.toggle("done", j < i);
    });
  }

  function spawn() {
    const c = state.target;
    const el = document.createElementNS("http://www.w3.org/2000/svg", "circle");
    el.setAttribute("r", "4");
    el.setAttribute("fill", color[c]);
    layer.append(el);
    packets.push({ el, route: ["p-in", "p-" + c[0], "p-" + c[0] + "db"], seg: 0, d: 0, speed: rand(0.16, 0.22) });
  }

  let last = performance.now(), acc = 0;
  function frame(t) {
    const dt = Math.min(50, t - last); last = t;
    if (visible.get(card) && !reduced) {
      acc += dt;
      if (acc > 240) { acc = 0; spawn(); }
      for (let i = packets.length - 1; i >= 0; i--) {
        const k = packets[i];
        k.d += k.speed * dt;
        let seg = paths[k.route[k.seg]];
        while (k.d > seg.len) {
          k.d -= seg.len; k.seg++;
          if (k.seg >= k.route.length) break;
          seg = paths[k.route[k.seg]];
        }
        if (k.seg >= k.route.length) { k.el.remove(); packets.splice(i, 1); continue; }
        const pt = seg.p.getPointAtLength(k.d);
        k.el.setAttribute("cx", pt.x);
        k.el.setAttribute("cy", pt.y);
      }
    }
    requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);

  let skip = null;
  replay.addEventListener("click", () => skip && skip());
  const waitOrReplay = (ms) => new Promise((r) => { const t = setTimeout(r, ms); skip = () => { clearTimeout(t); r(); }; });

  setSlot("blue", "live", 2);
  setSlot("green", "idle", 3);
  stepTo(-1);

  (async () => {
    for (;;) {
      const cur = state.live, nxt = other(cur);
      const v = state.ver[cur], nv = v + 1;
      status(`● serving v${v}`);
      replay.disabled = false;
      await waitOrReplay(3200);
      await whenVisible(card);
      replay.disabled = true; skip = null;

      stepTo(0); status(`◌ building v${nv} on your laptop`, "busy"); await pause(card, 1700);
      stepTo(1); status("◌ uploading changed layers", "busy"); await pause(card, 1400);
      stepTo(2); status(`◌ starting v${nv} next to v${v}`, "busy");
      state.ver[nxt] = nv; setSlot(nxt, "", nv); await pause(card, 1600);
      stepTo(3); status(`◌ health check on v${nv}`, "busy");
      setSlot(nxt, "checking"); await pause(card, 2200);
      setSlot(nxt, "");
      stepTo(4); status(`● v${nv} is live`);
      state.target = nxt; state.live = nxt;
      setSlot(nxt, "live"); setSlot(cur, "draining");
      await pause(card, 1600);
      stepTo(5); status(`◌ v${v} draining — in-flight requests finish`, "busy");
      await pause(card, 2200);
      setSlot(cur, "idle");
      stepTo(6);
      status(`● v${nv} is live · zero downtime`);
      await pause(card, 2400);
      stepTo(-1);
    }
  })();
})();

// ───────── Pull the plug ─────────
(function plug() {
  const btn = $("#plug-btn"), server = $("#server"), log = $("#boot-log");
  const sites = $$("#sites li");
  const text = $(".plug-text", btn);
  log.innerHTML = `<span class="ln">${muted(`uptime 182 days · ${sites.length} apps · all healthy`)}</span>`;
  const stamp = (s) => faint(`[${s.toFixed(1).padStart(4, " ")}s]`);

  btn.addEventListener("click", async () => {
    btn.disabled = true;
    log.innerHTML = "";
    server.classList.add("off");
    sites.forEach((li) => { li.classList.add("down"); li.classList.remove("up"); $(".site-state", li).textContent = "—"; });
    await sleep(1600);
    server.classList.remove("off");
    server.classList.add("booting");
    const L = (t, html) => line(log, `${stamp(t)} ${html}`);
    L(0, "⚡ power restored");
    await sleep(800);
    L(2.1, `systemd: ${muted("docker.service")} up`);
    await sleep(550);
    L(2.4, `caddy: last-known-good config`);
    await sleep(550);
    L(2.9, `<span class="c-acc">dokwalt</span>: reconciling ${sites.length} apps`);
    for (const li of sites) {
      await sleep(rand(380, 650));
      li.classList.remove("down");
      li.classList.add("up");
      $(".site-state", li).textContent = "200 OK";
    }
    L(4.4, `${ok} all sites answer over HTTPS`);
    server.classList.remove("booting");
    text.textContent = "Pull it again";
    btn.disabled = false;
  });
})();

// ───────── Releases & rollback ─────────
(function releases() {
  const box = $("#rel-rows");
  const card = box.closest(".card");
  track(card);
  const descs = ["Dark mode", "Faster checkout", "Bump Django to 5.2", "New pricing page", "Fix typo in footer", "Search filters"];
  const status = { live: '<span class="st-live">● live</span>', sup: '<span class="st-sup">● superseded</span>', dep: '<span class="st-dep">◌ deploying</span>' };
  let rows;
  const reset = () => {
    rows = [
      { v: 5, s: "live", d: "Faster checkout" },
      { v: 4, s: "sup", d: "Set SMTP_HOST, SMTP_PORT" },
      { v: 3, s: "sup", d: "New pricing page" },
      { v: 2, s: "sup", d: "Bump Django to 5.2" },
    ];
  };
  const render = (fresh) => {
    box.innerHTML = rows.map((r, i) => `<div class="rel-row${i === 0 && fresh ? " flash" : ""}" style="${i === 0 && fresh ? "" : "animation:none"}"><span>${r.s === "live" ? '<span class="c-ok">▶ v' + r.v + "</span>" : "&nbsp;&nbsp;v" + r.v}</span><span>${status[r.s]}</span><span>${r.d}</span></div>`).join("");
  };
  reset(); render(false);
  (async () => {
    let n = 0;
    for (;;) {
      await pause(card, 2600);
      // deploy
      const top = rows[0].v + 1;
      rows.unshift({ v: top, s: "dep", d: descs[n++ % descs.length] });
      rows = rows.slice(0, 5); render(true);
      await pause(card, 1500);
      rows[1].s = "sup"; rows[0].s = "live"; render(false);
      await pause(card, 2600);
      // rollback
      const back = rows[1].v;
      rows[0].s = "sup";
      rows.unshift({ v: top + 1, s: "live", d: `Rollback to v${back}` });
      rows = rows.slice(0, 5); render(true);
      if (top > 14) { await pause(card, 2600); reset(); render(false); }
    }
  })();
})();

// ───────── Pipeline promote ─────────
(function pipeline() {
  const chip = $(".chip"), prod = $("#prod-ver");
  const card = chip.closest(".card");
  const stagingVer = $(".stage b");
  track(card);
  let p = 6, s = 8;
  (async () => {
    for (;;) {
      await pause(card, 1800);
      chip.textContent = "sha:" + Math.random().toString(16).slice(2, 6) + "…";
      chip.classList.remove("go"); void chip.offsetWidth; chip.classList.add("go");
      await sleep(1500);
      prod.textContent = "v" + ++p;
      const st = prod.parentElement;
      st.classList.remove("bump"); void st.offsetWidth; st.classList.add("bump");
      await pause(card, 1800);
      stagingVer.textContent = "v" + ++s;
    }
  })();
})();

// ───────── Compose annotations ─────────
(function yaml() {
  const lines = $$(".y-line");
  const card = lines[0].closest(".card");
  track(card);
  (async () => {
    for (;;) {
      for (const l of lines) { await pause(card, 1100); l.classList.add("on"); }
      await pause(card, 2600);
      lines.forEach((l) => l.classList.remove("on"));
    }
  })();
})();

// ───────── HTTPS ─────────
(function https() {
  const host = $("#https-host"), box = host.closest(".https"), card = box.closest(".card");
  const marks = $$(".https-steps span", box);
  track(card);
  const hosts = ["shop.example.com", "blog.example.com", "api.example.com"];
  (async () => {
    for (let i = 0; ; i++) {
      const h = hosts[i % hosts.length];
      await whenVisible(card);
      for (const ch of h) { host.textContent += ch; await sleep(rand(40, 90)); }
      for (const m of marks) { await sleep(520); m.classList.add("ok"); }
      box.classList.add("secure");
      await pause(card, 2400);
      while (host.textContent) { host.textContent = host.textContent.slice(0, -1); await sleep(22); }
      marks.forEach((m) => m.classList.remove("ok"));
      box.classList.remove("secure");
      await sleep(400);
    }
  })();
})();

// ───────── Monitoring ─────────
(function monitor() {
  const bars = $("#bars"), logs = $("#logs"), card = bars.closest(".card");
  track(card);
  const N = 48;
  let vals = Array.from({ length: N }, (_, i) => 20 + 30 * Math.abs(Math.sin(i / 5)) + rand(0, 20));
  bars.innerHTML = vals.map((v) => `<i style="height:${v}%"></i>`).join("");
  const svc = [["web", "c-pink"], ["worker", "c-blue"], ["web", "c-pink"], ["cache", "c-ok"]];
  const reqs = ["GET /checkout 200 12ms", "GET / 200 4ms", "POST /cart 201 18ms", "GET /api/products 200 9ms", "GET /static/app.css 304 1ms"];
  const jobs = ["sent order confirmation #1284", "rebuilt search index (0.4s)", "processed 3 webhooks"];
  const lines = [];
  const pad = (n) => String(n).padStart(2, "0");
  let tick = 0;
  (async () => {
    for (;;) {
      await pause(card, 900);
      vals.shift(); vals.push(Math.max(8, Math.min(100, vals[vals.length - 1] + rand(-18, 18))));
      $$("i", bars).forEach((b, i) => (b.style.height = vals[i] + "%"));
      $("#m-rps").textContent = rand(9, 16).toFixed(1);
      $("#m-p95").textContent = Math.round(rand(8, 14)) + " ms";
      const d = new Date();
      const [name, cls] = svc[tick++ % svc.length];
      const msg = name === "worker" ? jobs[tick % jobs.length] : name === "cache" ? "* 1 changes in 60 seconds. Saving…" : reqs[tick % reqs.length];
      lines.push(`${faint(`${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`)} <span class="${cls}">${name.padEnd(6)}</span> ${faint("│")} ${msg}`);
      logs.innerHTML = lines.slice(-4).join("\n");
    }
  })();
})();

// ───────── Alerts ─────────
(function alerts() {
  const box = $("#alerts"), card = box.closest(".card");
  track(card);
  const seq = [
    { bad: true, t: "Site down", m: "shop.example.com is not answering: HTTP 502" },
    { bad: false, t: "Resolved", m: "shop.example.com answers again" },
    { bad: true, t: "Crash loop", m: "shop · worker restarted 5 times in 10 min" },
    { bad: true, t: "Disk", m: "prod disk is 91% full" },
    { bad: false, t: "Resolved", m: "prod disk back to 64%" },
  ];
  (async () => {
    for (let i = 0; ; i++) {
      await pause(card, 2200);
      const a = seq[i % seq.length];
      const el = document.createElement("div");
      el.className = "alert" + (a.bad ? "" : " ok");
      el.innerHTML = `<span class="av">◆</span><div><b>DokWalt · ${a.t}</b><small>${a.m}</small></div>`;
      box.append(el);
      while (box.children.length > 3) box.firstElementChild.remove();
    }
  })();
})();

// ───────── "Not in the box" strike-through ─────────
(function nope() {
  const list = $("#nope");
  $$("li", list).forEach((li) => {
    const t = li.firstChild;
    const span = document.createElement("span");
    span.textContent = t.textContent;
    li.replaceChild(span, t);
    li.style.setProperty("--sw", span.offsetWidth + "px");
  });
  const io = new IntersectionObserver(async ([e]) => {
    if (!e.isIntersecting) return;
    io.disconnect();
    for (const li of $$("li", list)) {
      li.style.setProperty("--sw", $("span", li).offsetWidth + "px");
      li.classList.add("struck");
      await sleep(reduced ? 0 : 230);
    }
  }, { threshold: 0.4 });
  io.observe(list);
})();

// ───────── TUI dashboard ─────────
(function dashboard() {
  const detail = $("#tui-detail"), wrap = detail.closest(".tui");
  const rows = $$(".app-row");
  track(wrap);
  const svcColor = { cache: "c-ok", migrate: "c-yellow", web: "c-pink", worker: "c-blue", db: "c-ok", search: "c-blue" };
  const apps = {
    "shop/production": {
      name: "shop", state: ["c-ok", "running"], url: "https://shop.example.test",
      svcs: [["cache", "1/1", 0.9, 9.1, '<span class="c-acc">keeps data</span>'], ["migrate", "✓ done", null, null, "runs once"], ["web", "1/1", 2.0, 11.3, "zero-downtime"]],
      traffic: ["shop.example.test", 12.3, 10], cpu: 2.9,
    },
    "shop/staging": {
      name: "shop", state: ["c-ok", "running"], url: "https://staging.shop.example.test",
      svcs: [["cache", "1/1", 0.4, 8.7, '<span class="c-acc">keeps data</span>'], ["migrate", "✓ done", null, null, "runs once"], ["web", "1/1", 0.8, 10.9, "zero-downtime"]],
      traffic: ["staging.shop.example.test", 0.4, 8], cpu: 1.2,
    },
    "blog/": {
      name: "blog", state: ["c-ok", "running"], url: "https://blog.example.test",
      svcs: [["db", "1/1", 0.3, 38.2, '<span class="c-acc">keeps data</span>'], ["web", "1/1", 0.9, 52.4, "zero-downtime"]],
      traffic: ["blog.example.test", 3.1, 22], cpu: 1.2,
    },
    "wiki/": {
      name: "wiki", state: ["c-yellow", "degraded"], url: "https://wiki.example.test",
      svcs: [["db", "1/1", 0.2, 41.0, '<span class="c-acc">keeps data</span>'], ["web", "1/1", 1.4, 88.1, "zero-downtime"], ["search", '<span class="c-red">0/1</span>', null, null, "zero-downtime"]],
      traffic: ["wiki.example.test", 1.8, 41], cpu: 1.6,
    },
  };
  let key = "shop/production";
  const spark = Array.from({ length: 48 }, (_, i) => (i < 6 ? 15 : i < 10 ? 35 : 55 + rand(-8, 10)));

  function render(anim) {
    const a = apps[key];
    const stage = key.split("/")[1];
    const j = (v, amt = 0.15) => (v * rand(1 - amt, 1 + amt)).toFixed(1);
    detail.innerHTML = `
      <div${anim ? ' class="fade-swap"' : ""}>
        <div>${a.name} <span class="${a.state[0]}">● ${a.state[1]}</span>${stage ? " " + muted(stage) : ""}</div>
        <div class="tui-tabs"><span class="on">Overview</span><span>│</span><span>Logs</span><span>│</span><span>Releases</span></div>
        <div class="c-blue">${a.url}</div>
        <div class="tui-table">
          <span class="h">SERVICE</span><span class="h">UP</span><span class="h">CPU</span><span class="h">MEM</span><span class="h hide-sm">ON DEPLOY</span>
          ${a.svcs.map(([n, up, cpu, mem, od]) => `<span class="${svcColor[n]}">${n}</span><span class="${up === "1/1" ? "c-ok" : ""}">${up}</span><span>${cpu == null ? "—" : j(cpu) + "%"}</span><span>${mem == null ? "—" : j(mem, 0.02) + " MiB"}</span><span class="hide-sm">${od}</span>`).join("")}
        </div>
        <div class="tui-table traffic">
          <span class="h">TRAFFIC</span><span class="h">REQ/S</span><span class="h">P95</span><span class="h">5XX/MIN</span>
          <span>${a.traffic[0]}</span><span>${j(a.traffic[1])}</span><span>${Math.round(a.traffic[2] * rand(0.85, 1.2))} ms</span><span>0</span>
        </div>
        <div class="spark-row"><span>CPU, last 10 min</span><span class="spark">${spark.map((v) => `<i style="height:${v}%"></i>`).join("")}</span></div>
      </div>`;
  }
  function select(row) {
    rows.forEach((r) => r.classList.toggle("active", r === row));
    key = `${row.dataset.app}/${row.dataset.stage}`;
    render(true);
  }
  rows.forEach((r) => r.addEventListener("click", () => select(r)));
  wrap.tabIndex = 0;
  wrap.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    e.preventDefault();
    const i = rows.findIndex((r) => r.classList.contains("active"));
    select(rows[(i + (e.key === "ArrowDown" ? 1 : rows.length - 1)) % rows.length]);
  });
  render(false);

  (async () => {
    for (;;) {
      await pause(wrap, 1200);
      spark.shift(); spark.push(Math.max(10, Math.min(100, spark[spark.length - 1] + rand(-12, 12))));
      const cpu = Math.round(rand(4, 11)), mem = Math.round(rand(12, 15));
      $("#g-cpu").style.width = cpu + "%"; $("#g-cpu-v").textContent = cpu + "%";
      $("#g-mem").style.width = mem + "%"; $("#g-mem-v").textContent = mem + "%";
      render(false);
    }
  })();
})();

// ───────── Command tour ─────────
(function tour() {
  const tabs = $("#tour-tabs"), body = $("#tour-body"), titleEl = $("#tour-title");
  const tourEl = tabs.closest(".tour");
  track(tourEl);
  const pad = (s, n) => s + " ".repeat(Math.max(0, n - s.length));
  const sup = '<span class="c-yellow">● superseded</span>';
  const C = [
    {
      cmd: "dokwalt releases", desc: "history of every release",
      run: async (t) => {
        line(t, muted(pad("RELEASE", 9) + pad("STATUS", 14) + pad("DESCRIPTION", 27) + pad("COMMIT", 10) + pad("CONFIG", 8) + "CREATED"));
        const rows = [
          ['<span class="c-ok">▶ v7</span>   ', '<span class="c-ok">● live</span>      ', "Rollback to v5", "4e1c9a2", "c4", "just now"],
          ["  v6   ", sup, "Dark mode", "9b07d1f", "c4", "4 min ago"],
          ["  v5   ", sup, "Faster checkout", "4e1c9a2", "c4", "2 hours ago"],
          ["  v4   ", sup, "Set SMTP_HOST, SMTP_PORT", "a83f2c0", "c4", "yesterday"],
          ["  v3   ", '<span class="c-red">● failed</span>    ', '<span class="c-red">web: health check failed</span>', "a83f2c0", "c3", "yesterday"],
        ];
        for (const r of rows) {
          await sleep(110);
          const plainDesc = r[2].replace(/<[^>]+>/g, "");
          line(t, `${r[0]}  ${r[1]}  ${r[2]}${" ".repeat(Math.max(1, 27 - plainDesc.length))}${muted(pad(r[3], 10))}${muted(pad(r[4], 8))}${muted(r[5])}`);
        }
      },
    },
    {
      cmd: "dokwalt rollback v5", desc: "instant, no rebuild",
      run: async (t, k) => {
        line(t, title("Rolling back shop"));
        await stepLine(t, "Containers running", 900, "1.1s", k);
        await stepLine(t, "web is healthy", 700, "", k);
        line(t, `${ok} Rolled back — v7 is live`);
      },
    },
    {
      cmd: "dokwalt config:set SMTP_HOST=smtp.resend.com SMTP_PORT=465", desc: "encrypted, zero-downtime",
      run: async (t, k) => {
        await stepLine(t, "Containers running", 900, "2.4s", k);
        await stepLine(t, "web is healthy", 700, "", k);
        line(t, `${ok} Config updated — shop restarted as v8 with zero downtime`);
      },
    },
    {
      cmd: "dokwalt promote", desc: "staging → production",
      run: async (t, k) => {
        line(t, `${title("Promoting shop")}${muted("  staging → production")}`);
        await stepLine(t, "Same images as staging v12 — nothing to build or upload", 500, "", k);
        await stepLine(t, "Containers running", 900, "1.9s", k);
        await stepLine(t, "web is healthy", 700, "", k);
        line(t, `${ok} Promoted — production is now v9`);
      },
    },
    {
      cmd: "dokwalt logs -f", desc: "merged, colored, live",
      run: async (t, k) => {
        const L = [
          ["web", "c-pink", "GET /checkout 200 12ms"], ["web", "c-pink", "POST /cart 201 18ms"],
          ["worker", "c-blue", "sent order confirmation #1284"], ["db", "c-ok", "checkpoint complete: wrote 42 buffers"],
          ["web", "c-pink", "GET /api/products 200 9ms"], ["worker", "c-blue", "rebuilt search index (0.4s)"],
          ["web", "c-pink", "GET / 200 4ms"], ["web", "c-pink", "GET /account 302 3ms"], ["worker", "c-blue", "processed 3 webhooks"],
        ];
        let s = 11;
        for (const [n, c, m] of L) {
          if (k.dead) return;
          await sleep(rand(250, 600));
          line(t, `${faint(`14:02:${String(s++).padStart(2, "0")}`)} <span class="${c}">${pad(n, 6)}</span> ${faint("│")} ${m}`);
        }
      },
    },
    {
      cmd: "dokwalt db:connect", desc: "psql over SSH, never public",
      run: async (t) => {
        line(t, title("Tunnel to db (postgres)"));
        await sleep(500);
        line(t, `${muted("•")} Connected to <b>db</b> (postgres) through SSH — exit the shell to close the tunnel`);
        await sleep(400);
        line(t, "psql (17.2)");
        line(t, 'Type "help" for help.\n');
        await sleep(300);
        await typeCmd(t, "select count(*) from orders;", null, "shop=#");
        await sleep(300);
        line(t, " count\n-------\n  1284\n(1 row)");
      },
    },
    {
      cmd: "dokwalt doctor", desc: "server, apps, DNS, your machine",
      run: async (t) => {
        line(t, title("Doctor") + "\n");
        const rows = [
          ["Local Docker", "Docker 28.4.0 (builds run here)"],
          ["Connection", "prod (deploy@203.0.113.7), daemon v0.1.1 on linux/arm64"],
          ["Docker", "Docker 29.8.2 (API 1.56), 4 CPUs, 7.9 GiB RAM"],
          ["Docker live-restore", "containers keep running while dockerd restarts or upgrades"],
          ["Start on boot: docker", "docker starts at boot"],
          ["Start on boot: dokwalt", "dokwalt starts at boot"],
          ["Clock", "synchronized with NTP"],
          ["DokWalt daemon", "build v0.1.1, 27.7 MiB RSS"],
          ["Proxy (Caddy)", "running, 45.1 MiB RSS, config loaded"],
          ["Domain shop.example.com", "ok"],
        ];
        for (const [a, b] of rows) { await sleep(90); line(t, `${ok} ${pad(a, 24)}${b}`); }
        line(t, `\n  ${rows.length} ok`);
      },
    },
  ];

  let current = 0, token = { dead: false };
  tabs.innerHTML = C.map((c, i) => `<button class="tour-tab" role="tab" aria-selected="${i === 0}" data-i="${i}" type="button"><code>${c.cmd.split(" ").slice(0, 2).join(" ")}</code><small>${c.desc}</small><span class="prog"></span></button>`).join("");
  const btns = $$(".tour-tab", tabs);

  async function show(i) {
    token.dead = true;
    token = { dead: false };
    const k = token;
    current = i;
    btns.forEach((b, j) => {
      b.setAttribute("aria-selected", String(j === i));
      b.classList.remove("counting");
    });
    titleEl.textContent = "~/code/shop — " + C[i].cmd.split(" ").slice(0, 2).join(" ");
    body.innerHTML = "";
    await typeCmd(body, C[i].cmd, k);
    if (k.dead) return;
    await sleep(200);
    await C[i].run(body, k);
    if (k.dead) return;
    const p = line(body, `${muted("$")} `);
    p.append(Object.assign(document.createElement("span"), { className: "cursor" }));
    // The progress bar under the tab is the timer: hovering pauses it.
    btns[i].classList.add("counting");
  }
  btns.forEach((b, i) => $(".prog", b).addEventListener("animationend", async () => {
    if (i !== current || !b.classList.contains("counting")) return;
    await whenVisible(tourEl);
    if (i === current) show((current + 1) % C.length);
  }));
  btns.forEach((b) => b.addEventListener("click", () => show(+b.dataset.i)));
  tourEl.addEventListener("pointerenter", () => tourEl.classList.add("paused"));
  tourEl.addEventListener("pointerleave", () => tourEl.classList.remove("paused"));
  whenVisible(tourEl).then(() => show(0));
})();
