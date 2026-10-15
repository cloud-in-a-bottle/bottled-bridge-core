import { openStorage } from "/static/storage.js";

// The app's branding and options (web.Branding).
const CONFIG = JSON.parse(document.getElementById("app-config").textContent);
const NETWORK = CONFIG.network;
const app = document.getElementById("app");
const PAGE_SIZE = 50;
const REACTIONS = ["❤️", "😆", "😮", "😢", "😠", "👍"];

const state = {
  account: null,
  conversations: [],
  activeId: null,
  detail: null,
  messages: [],
  hasMore: false,
  replyTo: null,
  editing: null,
  pendingFiles: [],
  highlight: null,
  typing: new Map(),
  // Whether the network has older history for the open chat (bridge.HistoryState).
  history: null,
  canSearchPeople: false,
};

// Listeners for server-sent events that extensions asked for, by kind.
const extensionListeners = new Map();

// ---------------------------------------------------------------- helpers

export async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body instanceof FormData) {
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (!res.ok) {
    let detail = `${res.status} ${res.statusText}`;
    try {
      const data = await res.json();
      detail = data.detail || detail;
    } catch {}
    throw new Error(detail);
  }
  if (res.status === 204) return null;
  const text = await res.text();
  return text ? JSON.parse(text) : null;
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function template(id) {
  return document.getElementById(id).content.cloneNode(true);
}

function avatarColor(seed) {
  // Golden-angle spacing so consecutive ids get clearly different hues.
  let h = 0;
  for (const ch of String(seed)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return `hsl(${Math.round((h * 137.508) % 360)} 45% 48%)`;
}

function avatar(name, seed, sha) {
  const initials = (name || "?").replace(/[^\p{L}\p{N} ]/gu, "").split(/\s+/).filter(Boolean)
    .slice(0, 2).map((w) => w[0].toUpperCase()).join("") || "#";
  const node = el("span", { class: "avatar", style: `background:${avatarColor(seed)}` }, initials);
  if (sha) {
    const img = el("img", { src: `/api/files/${sha}`, alt: "", loading: "lazy" });
    img.addEventListener("error", () => img.remove());
    node.replaceChildren(img);
  }
  return node;
}

function formatTime(ms) {
  const d = new Date(ms);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (now - d < 6 * 864e5) return d.toLocaleDateString([], { weekday: "short" });
  return d.toLocaleDateString([], { month: "short", day: "numeric", year: d.getFullYear() === now.getFullYear() ? undefined : "numeric" });
}

function formatDay(ms) {
  return new Date(ms).toLocaleDateString([], { weekday: "long", month: "long", day: "numeric", year: "numeric" });
}

function formatDuration(ms) {
  const seconds = Math.round(ms / 1000);
  if (!seconds) return "off";
  const units = [["w", 604800], ["d", 86400], ["h", 3600], ["m", 60], ["s", 1]];
  for (const [u, s] of units) if (seconds % s === 0) return `${seconds / s}${u}`;
  return `${seconds}s`;
}

function formatSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

function toast(message) {
  alert(message);
}

// ---------------------------------------------------------------- boot / login

async function boot() {
  const s = await api("GET", "/api/state");
  state.account = s.account;
  state.canSearchPeople = s.can_search_people;
  if (!s.account.logged_in) return renderSetup(s);
  renderMain();
}

function renderSetup(s) {
  if (eventSource) eventSource.close();
  eventSource = null;
  app.className = "";
  app.replaceChildren(template("tpl-setup"));
  app.querySelector('[data-field="title"]').textContent = CONFIG.title;
  app.querySelector('[data-field="login-heading"]').textContent = `Log in to ${NETWORK}`;
  app.querySelector('[data-field="login-intro"]').textContent = CONFIG.login_intro;
  const flows = app.querySelector("#login-flows");
  // Flows the app lists come first, in its order and wording.
  const rank = (f) => {
    const i = CONFIG.login_flows.findIndex((l) => l.id === f.id);
    return i < 0 ? CONFIG.login_flows.length : i;
  };
  const known = s.login_flows.slice().sort((a, b) => rank(a) - rank(b));
  flows.replaceChildren(...known.map((f) => {
    const label = CONFIG.login_flows.find((l) => l.id === f.id);
    return el("button", { type: "button", onclick: () => startLogin(f.id) },
      label ? label.label : f.name, el("small", {}, label ? label.hint : f.description));
  }));
  if (s.login_step) renderLoginStep(s.login_step);
}

function setupError(err) {
  const error = app.querySelector("#setup-error");
  if (!error) return;
  error.hidden = !err;
  error.textContent = err ? err.message : "";
}

async function startLogin(flowId) {
  setupError(null);
  try {
    renderLoginStep(await api("POST", "/api/login/start", { flow_id: flowId }));
  } catch (err) { setupError(err); }
}

async function cancelLogin() {
  await api("POST", "/api/login/cancel").catch(() => {});
  setupError(null);
  app.querySelector("#login-choose").hidden = false;
  app.querySelector("#login-step").hidden = true;
}

async function advanceLogin(step) {
  if (step.type === "complete") {
    await boot();
    return;
  }
  renderLoginStep(step);
}

function renderLoginStep(step) {
  const box = app.querySelector("#login-step");
  app.querySelector("#login-choose").hidden = true;
  box.hidden = false;
  const actions = el("div", { class: "step-actions" });
  const cancel = el("button", { type: "button", class: "link", onclick: cancelLogin }, "Cancel");
  const submit = async (fields, button) => {
    button.disabled = true;
    setupError(null);
    try {
      await advanceLogin(await api("POST", "/api/login/submit", { fields }));
    } catch (err) { setupError(err); } finally { button.disabled = false; }
  };

  if (step.type === "cookies" && step.cookies && /^https:\/\//.test(step.cookies.url)) {
    const host = new URL(step.cookies.url).hostname;
    const site = host.replace(/^www\./, "");
    const names = step.cookies.fields.map((f) => f.id);
    const raw = el("textarea", { rows: 6, placeholder: `curl 'https://${host}/…' -H 'cookie: …'` });
    const go = el("button", { type: "submit" }, "Log in");
    const form = el("form", {
      onsubmit: (e) => {
        e.preventDefault();
        const individual = Object.fromEntries(names.map((n) => [n, form.querySelector(`[name="${n}"]`).value.trim()]));
        submit(raw.value.trim() ? { raw: raw.value } : individual, go);
      },
    },
    el("ol", { class: "howto" },
      el("li", {}, "In a desktop browser, log in at ", el("a", { href: step.cookies.url, target: "_blank", rel: "noopener" }, site), "."),
      el("li", {}, "Open the developer tools (F12), go to the Network tab, and reload the page."),
      el("li", {}, `Right-click the first request to ${host} and choose Copy › Copy as cURL (bash). Paste it below.`)),
    el("label", {}, "cURL command or cookies", raw),
    el("details", {},
      el("summary", {}, "Or enter the cookies one by one"),
      el("p", { class: "hint" }, `From the developer tools' Application (Chrome) or Storage (Firefox) tab, under Cookies › https://${host}.`),
      ...names.map((n) => el("label", {}, n, el("input", { name: n, autocomplete: "off" })))),
    el("p", { class: "hint" }, "Don't log out of that browser session afterwards: that would also sign this server out. "
      + "Closing the tab is fine."),
    actions);
    actions.append(go, cancel);
    box.replaceChildren(el("h2", {}, `Log in with ${site} cookies`), form);
    return;
  }

  if (step.type === "user_input") {
    const fields = step.user_input.fields;
    const go = el("button", { type: "submit" }, "Continue");
    const form = el("form", {
      onsubmit: (e) => {
        e.preventDefault();
        submit(Object.fromEntries(fields.map((f) => [f.id, form.querySelector(`[name="${f.id}"]`).value])), go);
      },
    });
    if (step.instructions) form.append(el("p", {}, step.instructions));
    for (const f of fields) {
      let input;
      if (f.type === "select") {
        input = el("select", { name: f.id }, ...(f.options || []).map((o) => el("option", { value: o }, o)));
      } else {
        const type = { password: "password", email: "email", phone_number: "tel" }[f.type] || "text";
        input = el("input", {
          name: f.id, type, required: true, value: f.default_value || null,
          autocomplete: { password: "current-password", username: "username", "2fa_code": "one-time-code" }[f.type] || "off",
          inputmode: f.type === "2fa_code" ? "numeric" : null,
        });
      }
      form.append(el("label", {}, f.name, f.description ? el("small", { class: "hint" }, ` ${f.description}`) : null, input));
    }
    actions.append(go, cancel);
    form.append(actions);
    box.replaceChildren(el("h2", {}, "Log in"), form);
    return;
  }

  if (step.type === "display_and_wait") {
    const d = step.display_and_wait || {};
    let shown = null;
    if (d.type === "code" || d.type === "emoji") shown = el("p", { class: "display-code" }, d.data || "");
    // The server renders the step's QR code; the timestamp makes each new code (they rotate) load afresh.
    if (d.type === "qr") shown = el("img", { class: "login-qr", src: `/api/login/qr?t=${Date.now()}`, alt: "QR code" });
    box.replaceChildren(el("h2", {}, "Waiting…"), el("p", {}, step.instructions || "Waiting for confirmation."), shown, actions);
    actions.append(cancel);
    api("POST", "/api/login/wait").then(advanceLogin, (err) => {
      if (app.querySelector("#login-step") === box) setupError(err);
    });
    return;
  }

  // Steps that need a full browser on this server (captchas, Google sign-in) can't be done from here.
  box.replaceChildren(
    el("h2", {}, "Can't continue this login here"),
    el("p", {}, step.instructions || `${NETWORK} asked for a verification step this app can't show.`),
    CONFIG.login_fallback_hint ? el("p", { class: "hint" }, CONFIG.login_fallback_hint) : null,
    actions);
  actions.append(cancel);
}

// ---------------------------------------------------------------- main layout

function renderMain() {
  app.className = "";
  app.replaceChildren(template("tpl-main"));
  app.querySelector('[data-action="settings"]').addEventListener("click", openSettings);
  app.querySelector('[data-action="new-chat"]').addEventListener("click", openNewChat);
  app.querySelector('[data-action="storage"]').addEventListener("click", () => openStorage(api, NETWORK));
  const search = app.querySelector("#search");
  let searchTimer;
  search.addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => runSearch(search.value.trim()), 250);
  });
  renderConnection();
  connectEvents();
  refreshConversations();
  window.addEventListener("hashchange", routeFromHash);
  routeFromHash();
}

const STATE_TEXT = {
  CONNECTED: null,
  CONNECTING: `Connecting to ${NETWORK}…`,
  BACKFILLING: null,
  TRANSIENT_DISCONNECT: "Connection lost, reconnecting…",
  BAD_CREDENTIALS: `${NETWORK} signed this server out; your chats here are kept.`,
  UNKNOWN_ERROR: `${NETWORK} connection error.`,
  LOGGED_OUT: `Logged out of ${NETWORK}.`,
};

function renderConnection() {
  const box = app.querySelector("#connection");
  if (!box || !state.account) return;
  const a = state.account;
  const text = a.state in STATE_TEXT ? STATE_TEXT[a.state] : (a.state ? `${NETWORK}: ${a.state}` : "Starting…");
  box.hidden = !text;
  box.className = `connection${["BAD_CREDENTIALS", "UNKNOWN_ERROR", "LOGGED_OUT"].includes(a.state) ? " bad" : ""}`;
  box.textContent = text ? `${text}${a.state_message ? ` (${a.state_message})` : ""}` : "";
  if (a.state === "BAD_CREDENTIALS") {
    box.append(" ", el("button", { type: "button", class: "link", onclick: relogin }, "Log in again"));
  }
}

async function relogin() {
  try {
    await api("POST", "/api/logout");
    location.hash = "";
    await boot();
  } catch (err) { toast(err.message); }
}

function routeFromHash() {
  const m = location.hash.match(/^#\/c\/(\d+)(?:\/m\/(\d+))?/);
  if (m) openConversation(Number(m[1]), m[2] ? Number(m[2]) : null);
  else closeConversation();
}

async function refreshConversations() {
  state.conversations = await api("GET", "/api/conversations");
  renderConversationList();
}

// A DM's color follows the person, so it matches their avatar in the new-chat list.
function avatarSeed(c) {
  return c.other_person_id ? `p${c.other_person_id}` : `c${c.id}`;
}

function isMuted(c) {
  return c.muted_until === -1 || c.muted_until > Date.now();
}

function renderConversationList() {
  const list = app.querySelector("#conversations");
  if (!list) return;
  list.replaceChildren(...state.conversations.filter((c) => !c.archived || c.id === state.activeId).map((c) => el("li",
    { class: `${c.id === state.activeId ? "active" : ""}${isMuted(c) ? " muted" : ""}`, onclick: () => { location.hash = `#/c/${c.id}`; } },
    avatar(c.name, avatarSeed(c), c.avatar_sha256),
    el("span", { class: "name" }, c.pinned ? "📌 " : "", c.name),
    el("span", { class: "time" }, c.last_message_at ? formatTime(c.last_message_at) : ""),
    el("span", { class: "preview" }, state.typing.has(c.id) ? "typing…" : (c.last_message_preview || "")),
    c.unread_count ? el("span", { class: "unread" }, c.unread_count > 99 ? "99+" : c.unread_count) : (c.marked_unread ? el("span", { class: "unread" }, "•") : el("span")),
  )));
  if (!state.conversations.length) {
    list.append(el("li", { class: "hint" }, `No conversations yet. They appear here as ${NETWORK} syncs.`));
  }
}

async function runSearch(q) {
  const results = app.querySelector("#search-results");
  const convs = app.querySelector("#conversations");
  if (!q) {
    results.hidden = true;
    convs.hidden = false;
    return;
  }
  const hits = await api("GET", `/api/search?q=${encodeURIComponent(q)}`);
  convs.hidden = true;
  results.hidden = false;
  results.replaceChildren(...hits.map((h) => el("li",
    { onclick: () => { location.hash = `#/c/${h.conversation_id}/m/${h.message_id}`; } },
    avatar(h.conversation_name, `c${h.conversation_id}`),
    el("span", { class: "name" }, h.conversation_name),
    el("span", { class: "time" }, formatTime(h.sent_at)),
    el("span", { class: "preview" }, `${h.author_name}: `, ...highlightSnippet(h.snippet)),
  )));
  if (!hits.length) results.append(el("li", { class: "hint" }, "No matches."));
}

function highlightSnippet(snippet) {
  return snippet.split(/(\[[^\]]*\])/).map((part) =>
    part.startsWith("[") && part.endsWith("]") ? el("mark", {}, part.slice(1, -1)) : part);
}

// ---------------------------------------------------------------- conversation

function closeConversation() {
  state.activeId = null;
  const shell = app.querySelector(".shell");
  if (!shell) return;
  shell.classList.remove("in-thread");
  app.querySelector("#thread").replaceChildren(el("div", { class: "empty" }, "Select a conversation"));
  renderConversationList();
}

async function openConversation(id, highlightMessageId) {
  const switching = state.activeId !== id;
  state.activeId = id;
  state.highlight = highlightMessageId;
  app.querySelector(".shell").classList.add("in-thread");
  if (switching) {
    state.replyTo = null;
    state.editing = null;
    state.pendingFiles = [];
    state.history = null;
    const thread = app.querySelector("#thread");
    thread.replaceChildren(template("tpl-thread"));
    wireThread(thread);
  }
  state.detail = await api("GET", `/api/conversations/${id}`);
  renderThreadHeader();
  if (highlightMessageId) {
    const loc = await api("GET", `/api/messages/${highlightMessageId}/location`);
    // Load a page ending just after the target so it is on screen.
    const page = await api("GET", `/api/conversations/${id}/messages?before=${loc.sent_at + 1}&limit=${PAGE_SIZE}`);
    const after = await api("GET", `/api/conversations/${id}/messages?limit=${PAGE_SIZE}`);
    state.messages = page.messages;
    state.hasMore = page.has_more;
    if (after.messages.length && after.messages[0].sent_at > loc.sent_at) {
      state.messages = state.messages.concat(after.messages.filter((m) => m.sent_at > loc.sent_at));
    }
  } else {
    const page = await api("GET", `/api/conversations/${id}/messages?limit=${PAGE_SIZE}`);
    state.messages = page.messages;
    state.hasMore = page.has_more;
  }
  renderMessages({ scroll: highlightMessageId ? "highlight" : "bottom" });
  renderConversationList();
  markRead();
  if (CONFIG.on_demand_history && !state.detail.summary.read_only) {
    const h = await api("GET", `/api/conversations/${id}/history`);
    if (id === state.activeId) {
      state.history = h;
      renderHistoryButton();
    }
  }
}

// Once everything stored here is shown, offer to fetch older messages from the network, a page at a time.
function renderHistoryButton() {
  const button = app.querySelector('[data-action="load-older"]');
  if (!button) return;
  const h = state.history;
  button.hidden = !h || state.hasMore || (!h.has_more && !h.loading);
  button.disabled = Boolean(h && h.loading);
  button.textContent = h && h.loading ? `Loading earlier messages from ${NETWORK}…` : `Load earlier messages from ${NETWORK}`;
}

async function loadOlder() {
  try {
    await api("POST", `/api/conversations/${state.activeId}/history`);
  } catch (err) { toast(err.message); }
}

function renderThreadHeader() {
  const d = state.detail;
  const thread = app.querySelector("#thread");
  thread.querySelector('[data-field="avatar"]').replaceChildren(
    avatar(d.summary.name, avatarSeed(d.summary), d.summary.avatar_sha256));
  thread.querySelector('[data-field="name"]').textContent = d.summary.name;
  const parts = [];
  if (d.summary.kind === "group") parts.push(`${d.members.length} members`);
  if (d.summary.disappear_timer_ms) parts.push(`⏱ ${formatDuration(d.summary.disappear_timer_ms)}`);
  if (isMuted(d.summary)) parts.push("muted");
  if (d.summary.read_only) parts.push("imported history only");
  thread.querySelector("#composer").hidden = d.summary.read_only;
  thread.querySelector('[data-field="subtitle"]').textContent = parts.join(" · ");
}

function wireThread(thread) {
  thread.querySelector('[data-action="back"]').addEventListener("click", () => { location.hash = ""; });
  thread.querySelector('[data-action="load-more"]').addEventListener("click", loadMore);
  thread.querySelector('[data-action="load-older"]').addEventListener("click", loadOlder);
  thread.querySelector('[data-action="cancel-reply"]').addEventListener("click", () => { setReply(null); setEditing(null); });
  const input = thread.querySelector("#composer-input");
  const fileInput = thread.querySelector("#file-input");
  let typingSent = 0;
  let typingStopTimer;
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = `${input.scrollHeight}px`;
    if (state.editing) return;
    const now = Date.now();
    if (input.value && now - typingSent > 8000) {
      typingSent = now;
      api("POST", `/api/conversations/${state.activeId}/typing`, { stop: false }).catch(() => {});
    }
    clearTimeout(typingStopTimer);
    typingStopTimer = setTimeout(() => {
      if (typingSent) {
        typingSent = 0;
        api("POST", `/api/conversations/${state.activeId}/typing`, { stop: true }).catch(() => {});
      }
    }, 4000);
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && window.matchMedia("(min-width: 761px)").matches) {
      e.preventDefault();
      thread.querySelector("#composer").requestSubmit();
    } else if (e.key === "Escape" && state.editing) {
      setEditing(null);
    }
  });
  input.addEventListener("paste", (e) => {
    const files = [...e.clipboardData.files];
    if (files.length) {
      e.preventDefault();
      addPendingFiles(files);
    }
  });
  fileInput.addEventListener("change", () => {
    addPendingFiles([...fileInput.files]);
    fileInput.value = "";
  });
  thread.querySelector("#composer").addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = input.value.trim();
    const btn = thread.querySelector(".send");
    if (state.editing) {
      if (!body) return;
      btn.disabled = true;
      try {
        await api("POST", `/api/messages/${state.editing.id}/edit`, { body });
        setEditing(null);
      } catch (err) {
        toast(`Edit failed: ${err.message}`);
      } finally { btn.disabled = false; }
      return;
    }
    if (!body && !state.pendingFiles.length) return;
    const form = new FormData();
    form.append("body", body);
    if (state.replyTo) form.append("quote_id", String(state.replyTo.id));
    for (const f of state.pendingFiles) form.append("files", f, f.name);
    btn.disabled = true;
    try {
      await api("POST", `/api/conversations/${state.activeId}/messages`, form);
      input.value = "";
      input.style.height = "auto";
      typingSent = 0;
      setReply(null);
      state.pendingFiles = [];
      renderPendingFiles();
    } catch (err) {
      toast(`Send failed: ${err.message}`);
    } finally {
      btn.disabled = false;
      input.focus();
    }
  });
}

function addPendingFiles(files) {
  state.pendingFiles.push(...files);
  renderPendingFiles();
}

function renderPendingFiles() {
  const box = app.querySelector("#pending-files");
  if (!box) return;
  box.hidden = !state.pendingFiles.length;
  box.replaceChildren(...state.pendingFiles.map((f, i) => el("span", {},
    f.name, " ", el("button", { type: "button", class: "icon", onclick: () => {
      state.pendingFiles.splice(i, 1);
      renderPendingFiles();
    } }, "✕"))));
}

function showComposerBar(label, author, text) {
  const bar = app.querySelector("#reply-bar");
  if (!bar) return;
  bar.hidden = !label;
  if (!label) return;
  bar.querySelector('[data-field="reply-label"]').replaceChildren(label, author ? el("span", {}, author) : "");
  bar.querySelector('[data-field="reply-text"]').textContent = text;
  app.querySelector("#composer-input").focus();
}

function setReply(message) {
  state.replyTo = message;
  if (message) state.editing = null;
  showComposerBar(message && "Replying to ", message && message.author_name, message ? (message.body || "(attachment)") : "");
}

function setEditing(message) {
  state.editing = message;
  const input = app.querySelector("#composer-input");
  if (message) {
    state.replyTo = null;
    input.value = message.body;
    input.dispatchEvent(new Event("input"));
  } else if (input) {
    input.value = "";
    input.style.height = "auto";
  }
  showComposerBar(message && "Editing message", null, message ? message.body : "");
}

async function loadMore() {
  if (!state.messages.length) return reloadActive();
  const page = await api("GET",
    `/api/conversations/${state.activeId}/messages?before=${state.messages[0].sent_at}&limit=${PAGE_SIZE}`);
  state.messages = page.messages.concat(state.messages);
  state.hasMore = page.has_more;
  renderMessages({ scroll: "keep" });
}

async function reloadActive() {
  if (state.activeId === null) return;
  const id = state.activeId;
  const [detail, newest] = await Promise.all([
    api("GET", `/api/conversations/${id}`),
    api("GET", `/api/conversations/${id}/messages?limit=${PAGE_SIZE}`),
  ]);
  if (id !== state.activeId) return;
  state.detail = detail;
  renderThreadHeader();
  // Replace the newest page in place, keeping any older messages already loaded.
  const oldestNew = newest.messages.length ? newest.messages[0].sent_at : Infinity;
  const older = state.messages.filter((m) => m.sent_at < oldestNew);
  state.messages = older.concat(newest.messages);
  if (!older.length) state.hasMore = newest.has_more;
  const box = app.querySelector("#messages");
  const atBottom = box && box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  renderMessages({ scroll: atBottom ? "bottom" : "keep" });
  if (document.visibilityState === "visible") markRead();
}

let markReadTimer;
function markRead() {
  clearTimeout(markReadTimer);
  const id = state.activeId;
  const c = state.conversations.find((x) => x.id === id);
  if (c && !c.unread_count && !c.marked_unread) return;
  markReadTimer = setTimeout(() => {
    if (id === state.activeId) api("POST", `/api/conversations/${id}/read`).catch(() => {});
  }, 500);
}

// ---------------------------------------------------------------- messages

function renderMessages({ scroll }) {
  const box = app.querySelector("#messages");
  const list = app.querySelector("#message-list");
  if (!box || !list) return;
  const prevHeight = box.scrollHeight;
  const prevTop = box.scrollTop;
  app.querySelector('[data-action="load-more"]').hidden = !state.hasMore;
  renderHistoryButton();
  const isGroup = state.detail && state.detail.summary.kind === "group";
  const items = [];
  let lastDay = null;
  let lastAuthor = null;
  for (const m of state.messages) {
    const day = new Date(m.sent_at).toDateString();
    if (day !== lastDay) {
      items.push(el("li", { class: "day" }, formatDay(m.sent_at)));
      lastDay = day;
      lastAuthor = null;
    }
    if (m.kind === "update") {
      const text = m.msgtype === "notice" ? m.body : `${m.outgoing ? "You" : m.author_name} ${m.body}`;
      items.push(el("li", { class: "update" }, text, " · ", formatTime(m.sent_at)));
      lastAuthor = null;
      continue;
    }
    const showAuthor = isGroup && !m.outgoing && lastAuthor !== m.author_id;
    items.push(renderMessage(m, showAuthor));
    lastAuthor = m.author_id;
  }
  closePopover();
  list.replaceChildren(...items);
  if (scroll === "bottom") box.scrollTop = box.scrollHeight;
  else if (scroll === "keep") box.scrollTop = prevTop + (box.scrollHeight - prevHeight);
  else if (scroll === "highlight") {
    const target = list.querySelector(".msg.highlight");
    if (target) target.scrollIntoView({ block: "center" });
  }
}

const STATUS_TEXT = { pending: "Sending…", sent: "✓", delivered: "✓✓", read: "✓✓ Seen" };

function renderMessage(m, showAuthor) {
  const node = el("li", { class: `msg${m.outgoing ? " out" : ""}${m.id === state.highlight ? " highlight" : ""}` });
  if (showAuthor) node.append(el("span", { class: "author" }, m.author_name));
  const onlySticker = !m.deleted && !m.body && m.attachments.length === 1 && m.attachments[0].is_sticker;
  const bubble = el("div", { class: `bubble${onlySticker ? " sticker" : ""}${m.msgtype === "m.notice" ? " notice" : ""}` });
  if (m.deleted) {
    bubble.classList.add("deleted");
    bubble.append(m.outgoing ? "You unsent a message." : "This message was unsent.");
  } else {
    if (m.quote) {
      bubble.append(el("div", {
        class: "quote",
        onclick: () => { location.hash = `#/c/${state.activeId}/m/${m.quote.message_id}`; },
      }, el("strong", {}, m.quote.author_name), m.quote.text || "(attachment)"));
    }
    for (const a of m.attachments) bubble.append(renderAttachment(a));
    if (m.body) bubble.append(el("div", { class: "text" }, ...linkify(m.body)));
    for (const p of parsePreviews(m.link_previews)) bubble.append(renderLinkPreview(p));
    if (!m.body && !m.attachments.length) bubble.append(el("span", { class: "missing" }, "(empty message)"));
  }
  const row = el("div", { class: "msg-row" }, bubble);
  if (!m.deleted && !(state.detail && state.detail.summary.read_only)) row.append(renderTools(m, node));
  node.append(row);
  if (m.reactions.length) {
    const groups = new Map();
    for (const r of m.reactions) {
      if (!groups.has(r.emoji)) groups.set(r.emoji, []);
      groups.get(r.emoji).push(r);
    }
    node.append(el("div", { class: "reactions" }, [...groups].map(([emoji, rs]) => el("span", {
      class: rs.some((r) => r.mine) ? "mine" : "", title: rs.map((r) => (r.mine ? "You" : r.reactor_name)).join(", "),
    }, emoji, rs.length > 1 ? ` ${rs.length}` : ""))));
  }
  const meta = [formatTime(m.sent_at)];
  if (m.edited_at) meta.push("edited");
  if (m.expires_at) meta.push(`⏱ ${formatDuration(Math.max(0, m.expires_at - Date.now()))}`);
  const metaNode = el("div", { class: "meta" }, meta.join(" · "));
  if (m.status === "failed") {
    const act = (path) => api("POST", `/api/messages/${m.id}/${path}`).catch((e) => toast(e.message));
    metaNode.append(
      el("span", { class: "failed", title: m.error || "" }, `· Not sent${m.error ? `: ${m.error}` : ""}`),
      el("button", { type: "button", class: "link", onclick: () => act("retry") }, "Retry"),
      el("button", { type: "button", class: "link", onclick: () => act("discard") }, "Discard"));
  } else if (m.status) {
    metaNode.append(`· ${STATUS_TEXT[m.status] || m.status}`);
  }
  node.append(metaNode);
  // On touch screens there's no hover: tapping a message shows its tools.
  node.addEventListener("click", (e) => {
    if (window.matchMedia("(hover: none)").matches && !e.target.closest("a,button,audio,video,img,.quote")) {
      for (const other of document.querySelectorAll(".msg.show-tools")) if (other !== node) other.classList.remove("show-tools");
      node.classList.toggle("show-tools");
    }
  });
  return node;
}

function parsePreviews(raw) {
  if (!raw) return [];
  return Array.isArray(raw) ? raw : [];
}

function renderLinkPreview(p) {
  const url = p.matched_url || p["og:url"];
  if (!url || !/^https?:\/\//.test(url)) return null;
  const image = /^mxc:\/\/fb\.local\/([0-9a-f]{64})$/.exec(p["og:image"] || "");
  return el("a", { class: "link-preview", href: url, target: "_blank", rel: "noopener noreferrer" },
    image ? el("img", { src: `/api/files/${image[1]}`, alt: "", loading: "lazy" }) : null,
    el("strong", {}, p["og:title"] || url),
    p["og:description"] ? el("small", {}, p["og:description"]) : null);
}

const ICONS = {
  more: '<svg viewBox="0 0 24 24" width="20" height="20"><circle cx="12" cy="5" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="12" cy="19" r="2"/></svg>',
  reply: '<svg viewBox="0 0 24 24" width="20" height="20"><path d="M10 9V5l-7 7 7 7v-4.1c5 0 8.5 1.6 11 5.1-1-5-4-10-11-11z"/></svg>',
  react: '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.8"><circle cx="12" cy="12" r="9"/><path d="M8.5 14.5c1.9 2 5.1 2 7 0" stroke-linecap="round"/><circle cx="9" cy="10" r="1.1" fill="currentColor" stroke="none"/><circle cx="15" cy="10" r="1.1" fill="currentColor" stroke="none"/></svg>',
};

function toolButton(name, title, onclick) {
  const button = el("button", { type: "button", class: "tool", title, "aria-label": title, onclick });
  button.innerHTML = ICONS[name];
  return button;
}

// The icons beside a bubble, as messenger.com has them: react, reply, and a menu of the rest. In the DOM they follow the
// bubble; for outgoing messages the row is reversed, so they sit to its left.
function renderTools(m, node) {
  const tools = el("div", { class: "tools" });
  tools.append(
    toolButton("react", "React", (e) => openPopover(e.currentTarget, node, reactionPicker(m))),
    toolButton("reply", "Reply", () => setReply(m)),
  );
  const items = menuItems(m);
  if (items.length) tools.append(toolButton("more", "More", (e) => openPopover(e.currentTarget, node, menu(items))));
  return tools;
}

function reactionPicker(m) {
  const mine = m.reactions.find((r) => r.mine);
  return el("div", { class: "reaction-picker" }, REACTIONS.map((emoji) => el("button", {
    type: "button",
    class: mine && mine.emoji === emoji ? "mine" : null,
    title: mine && mine.emoji === emoji ? "Remove reaction" : `React ${emoji}`,
    onclick: () => {
      closePopover();
      api("POST", `/api/messages/${m.id}/reaction`, { emoji: mine && mine.emoji === emoji ? null : emoji })
        .catch((e) => toast(e.message));
    },
  }, emoji)));
}

function menuItems(m) {
  const items = [];
  const sent = m.outgoing && m.status !== "pending" && m.status !== "failed";
  if (sent && m.body && !m.attachments.length) items.push(["Edit", () => setEditing(m)]);
  if (sent) {
    items.push(["Unsend", async () => {
      if (!confirm("Unsend this message for everyone?")) return;
      await api("DELETE", `/api/messages/${m.id}`).catch((e) => toast(e.message));
    }]);
  }
  if (m.body) items.push(["Copy text", () => navigator.clipboard.writeText(m.body).catch((e) => toast(e.message))]);
  return items;
}

function menu(items) {
  return el("div", { class: "menu" }, items.map(([label, action]) => el("button", {
    type: "button", onclick: () => { closePopover(); action(); },
  }, label)));
}

// One popover at a time, above the button that opened it. It stays until something in it is chosen, or a click
// elsewhere, Escape or scrolling closes it; its message keeps showing its tools meanwhile.
let popover = null;

function openPopover(anchor, owner, content) {
  const reopening = popover && popover.anchor === anchor;
  closePopover();
  if (reopening) return;
  const box = el("div", { class: "popover" }, content);
  document.body.append(box);
  owner.classList.add("tools-open");
  popover = { box, anchor, owner };
  const a = anchor.getBoundingClientRect();
  const b = box.getBoundingClientRect();
  const left = Math.min(Math.max(8, a.left + a.width / 2 - b.width / 2), window.innerWidth - b.width - 8);
  const above = a.top - b.height - 10 >= 8;
  box.style.left = `${left}px`;
  box.style.top = `${above ? a.top - b.height - 10 : a.bottom + 10}px`;
  box.classList.add(above ? "above" : "below");
  box.style.setProperty("--arrow-x", `${a.left + a.width / 2 - left}px`);
}

function closePopover() {
  if (!popover) return;
  popover.box.remove();
  popover.owner.classList.remove("tools-open");
  popover = null;
}

document.addEventListener("mousedown", (e) => {
  if (popover && !popover.box.contains(e.target) && !popover.anchor.contains(e.target)) closePopover();
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closePopover(); });
document.addEventListener("scroll", () => closePopover(), true);

function renderAttachment(a) {
  const url = `/api/attachments/${a.id}`;
  const type = a.content_type || "";
  if (a.status !== "present") {
    const label = a.status === "purged" ? "Media removed" : "Media not available";
    return el("div", { class: "missing" }, `${label}${a.filename ? ` (${a.filename})` : ""}`);
  }
  let media;
  if (a.msgtype === "m.image" || a.msgtype === "m.sticker" || (type.startsWith("image/") && a.msgtype !== "m.file")) {
    media = el("a", { href: url, target: "_blank", rel: "noopener" },
      el("img", { src: url, loading: "lazy", alt: a.filename || "image", width: a.width || null, height: a.height || null }));
  } else if (a.msgtype === "m.video" || type.startsWith("video/")) {
    media = el("video", { src: url, controls: true, preload: "metadata" });
  } else if (a.msgtype === "m.audio" || type.startsWith("audio/")) {
    media = el("audio", { src: url, controls: true, preload: "metadata" });
  } else {
    media = el("a", { class: "file", href: url, download: a.filename || "" },
      `📄 ${a.filename || "file"}${a.size ? ` (${formatSize(a.size)})` : ""}`);
  }
  return a.caption ? el("div", {}, media, el("div", {}, ...linkify(a.caption))) : media;
}

const URL_RE = /\bhttps?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]]/g;

function linkify(text) {
  const out = [];
  let last = 0;
  for (const match of text.matchAll(URL_RE)) {
    if (match.index > last) out.push(text.slice(last, match.index));
    out.push(el("a", { href: match[0], target: "_blank", rel: "noopener noreferrer" }, match[0]));
    last = match.index + match[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

// ---------------------------------------------------------------- live events

let eventSource = null;

function connectEvents() {
  if (eventSource) eventSource.close();
  const source = new EventSource("/api/events");
  eventSource = source;
  let refreshTimer;
  const refreshSoon = () => {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(refreshConversations, 150);
  };
  let reloadTimer;
  const reloadSoon = () => {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(reloadActive, 100);
  };
  source.addEventListener("conversation", (e) => {
    const { conversationId } = JSON.parse(e.data);
    if (conversationId === state.activeId) reloadSoon();
    refreshSoon();
  });
  source.addEventListener("typing", (e) => {
    const { conversationId, started } = JSON.parse(e.data);
    clearTimeout(state.typing.get(conversationId));
    if (started) state.typing.set(conversationId, setTimeout(() => {
      state.typing.delete(conversationId);
      renderTyping();
      renderConversationList();
    }, 8000));
    else state.typing.delete(conversationId);
    renderTyping();
    renderConversationList();
  });
  source.addEventListener("status", (e) => {
    const s = JSON.parse(e.data);
    state.account = { ...state.account, state: s.state_event, state_error: s.error, state_message: s.message };
    renderConnection();
  });
  source.addEventListener("account", () => boot());
  source.addEventListener("history", (e) => {
    const h = JSON.parse(e.data);
    if (h.conversation_id !== state.activeId) return;
    const wasLoading = state.history && state.history.loading;
    state.history = h;
    if (wasLoading && !h.loading) loadMore().catch((err) => toast(err.message));
    else renderHistoryButton();
  });
  for (const ext of CONFIG.extensions) {
    for (const kind of ext.events) {
      source.addEventListener(kind, (e) => {
        const data = JSON.parse(e.data);
        for (const fn of extensionListeners.get(kind) || []) fn(data);
      });
    }
  }
  // EventSource reconnects on its own; resync once it does in case events were missed.
  source.addEventListener("hello", async () => {
    refreshSoon();
    if (state.activeId !== null) reloadSoon();
    const s = await api("GET", "/api/state").catch(() => null);
    if (s && !s.account.logged_in) { source.close(); return boot(); }
    if (s) { state.account = s.account; renderConnection(); }
  });
}

function renderTyping() {
  const box = app.querySelector("#typing");
  if (!box) return;
  const typing = state.typing.has(state.activeId);
  box.hidden = !typing;
  box.textContent = typing ? "typing…" : "";
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && state.activeId !== null) markRead();
});

// ---------------------------------------------------------------- new chat

async function openNewChat() {
  const frag = template("tpl-new-chat");
  const backdrop = frag.querySelector(".modal-backdrop");
  document.body.append(frag);
  const close = () => backdrop.remove();
  backdrop.querySelector('[data-action="close"]').addEventListener("click", close);
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  const start = async (c) => {
    try {
      const target = c.person_id ? { person_id: c.person_id } : c.identifier ? { identifier: c.identifier } : { remote_id: c.remote_id };
      const id = c.conversation_id || (await api("POST", "/api/conversations", target)).id;
      close();
      await refreshConversations();
      location.hash = `#/c/${id}`;
    } catch (err) { toast(err.message); }
  };
  // People known only by name (from an import) or the bridge itself can't be messaged.
  const contacts = (await api("GET", "/api/contacts")).filter((c) => c.remote_id !== "bot" && !c.remote_id.startsWith("name:"));
  const list = backdrop.querySelector("#contacts");
  const label = backdrop.querySelector("#contacts-label");
  const filter = backdrop.querySelector("#contact-filter");
  const searchButton = backdrop.querySelector('[data-field="search-network"]');
  searchButton.textContent = `Search ${NETWORK}`;
  searchButton.hidden = !state.canSearchPeople;
  const elsewhere = state.canSearchPeople ? `To find anyone else, search ${NETWORK}.` : "";
  if (CONFIG.identifier_label) {
    const form = backdrop.querySelector("#identifier-form");
    const input = form.querySelector("#identifier-input");
    form.hidden = false;
    input.placeholder = CONFIG.identifier_placeholder || CONFIG.identifier_label;
    input.setAttribute("aria-label", CONFIG.identifier_label);
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (input.value.trim()) start({ identifier: input.value.trim() });
    });
  }
  const render = (f) => {
    f = f.toLowerCase();
    label.textContent = `People from your chats here. ${elsewhere}`;
    list.replaceChildren(...contacts
      .filter((c) => !f || c.name.toLowerCase().includes(f))
      .map((c) => el("li", { onclick: () => start(c) },
        avatar(c.name, `p${c.person_id}`, c.avatar_sha256), el("span", {}, c.name))));
    if (!list.children.length) {
      list.append(el("li", { class: "hint" }, state.canSearchPeople ? `No matches here; try searching ${NETWORK}.` : "No matches here."));
    }
  };
  filter.addEventListener("input", (e) => render(e.target.value));
  backdrop.querySelector("#people-search").addEventListener("submit", async (e) => {
    e.preventDefault();
    const q = filter.value.trim();
    if (!q) return;
    const button = e.target.querySelector("button");
    button.disabled = true;
    try {
      const found = await api("GET", `/api/people/search?q=${encodeURIComponent(q)}`);
      label.textContent = `On ${NETWORK}, for “${q}”:`;
      list.replaceChildren(...found.map((p) => el("li", { onclick: () => start(p) },
        avatar(p.name, `f${p.remote_id}`), el("span", {}, p.name, el("small", { class: "hint" },
          p.has_chat ? " · your chat" : p.is_contact ? " · connected" : p.context ? ` · ${p.context}` : " · not connected")))));
      if (!found.length) list.append(el("li", { class: "hint" }, "Nobody found."));
    } catch (err) { toast(err.message); } finally { button.disabled = false; }
  });
  render("");
}

// ---------------------------------------------------------------- settings

async function openSettings() {
  const frag = template("tpl-settings");
  const backdrop = frag.querySelector(".modal-backdrop");
  document.body.append(frag);
  const close = () => backdrop.remove();
  backdrop.querySelector('[data-action="close"]').addEventListener("click", close);
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  const s = await api("GET", "/api/state");
  const a = s.account;
  backdrop.querySelector('[data-field="name"]').textContent = a.name || "(unknown)";
  backdrop.querySelector('[data-field="remote-id"]').textContent = a.remote_id ? `ID ${a.remote_id}` : "";
  backdrop.querySelector('[data-field="state"]').textContent = a.state || "starting";
  backdrop.querySelector('[data-field="state-detail"]').textContent = [a.state_error, a.state_message].filter(Boolean).join(": ");
  backdrop.querySelector('[data-field="logout-hint"]').textContent = `Logging out signs this server out of ${NETWORK}. `
    + "Your stored messages stay here; logging in again (with the same account) picks up where it left off.";
  backdrop.querySelector('[data-field="history-help"]').textContent = CONFIG.history_help;
  const sections = backdrop.querySelector("#extension-sections");
  for (const ext of CONFIG.extensions) {
    const mod = await import(`/static/app/${ext.module}`);
    sections.append(mod.settingsSection({
      api,
      conversations: () => state.conversations,
      on: (kind, fn) => {
        if (!extensionListeners.has(kind)) extensionListeners.set(kind, new Set());
        extensionListeners.get(kind).add(fn);
        return () => extensionListeners.get(kind).delete(fn);
      },
    }));
  }
  backdrop.querySelector('[data-action="reconnect"]').addEventListener("click", async () => {
    try {
      await api("POST", "/api/reconnect");
      close();
    } catch (err) { toast(err.message); }
  });
  backdrop.querySelector('[data-action="logout"]').addEventListener("click", async () => {
    if (!confirm(`Log this server out of ${NETWORK}? Stored messages are kept.`)) return;
    try {
      await api("POST", "/api/logout");
      close();
      location.hash = "";
      await boot();
    } catch (err) { toast(err.message); }
  });
}

boot().catch((err) => {
  app.className = "loading";
  app.textContent = `Failed to load: ${err.message}`;
});
