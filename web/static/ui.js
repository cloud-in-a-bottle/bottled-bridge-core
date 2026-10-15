// Building blocks of the chat UI that don't depend on where its data comes from: this app's own API, or (in the
// inbox app) the messaging service. They render plain values; callers map their data to them.

export function el(tag, attrs = {}, ...children) {
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

export function template(id) {
  return document.getElementById(id).content.cloneNode(true);
}

function avatarColor(seed) {
  // Golden-angle spacing so consecutive ids get clearly different hues.
  let h = 0;
  for (const ch of String(seed)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return `hsl(${Math.round((h * 137.508) % 360)} 45% 48%)`;
}

// A round avatar: the image at src if there is one and it loads, else initials on a color picked by seed.
export function avatar(name, seed, src) {
  const initials = (name || "?").replace(/[^\p{L}\p{N} ]/gu, "").split(/\s+/).filter(Boolean)
    .slice(0, 2).map((w) => w[0].toUpperCase()).join("") || "#";
  const node = el("span", { class: "avatar", style: `background:${avatarColor(seed)}` }, initials);
  if (src) {
    const img = el("img", { src, alt: "", loading: "lazy" });
    img.addEventListener("error", () => img.remove());
    node.replaceChildren(img);
  }
  return node;
}

// Times are anything Date accepts: milliseconds or ISO 8601 strings.
export function formatTime(t) {
  const d = new Date(t);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (now - d < 6 * 864e5) return d.toLocaleDateString([], { weekday: "short" });
  return d.toLocaleDateString([], { month: "short", day: "numeric", year: d.getFullYear() === now.getFullYear() ? undefined : "numeric" });
}

export function formatDay(t) {
  return new Date(t).toLocaleDateString([], { weekday: "long", month: "long", day: "numeric", year: "numeric" });
}

export function formatDuration(ms) {
  const seconds = Math.round(ms / 1000);
  if (!seconds) return "off";
  const units = [["w", 604800], ["d", 86400], ["h", 3600], ["m", 60], ["s", 1]];
  for (const [u, s] of units) if (seconds % s === 0) return `${seconds / s}${u}`;
  return `${seconds}s`;
}

export function formatSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
}

export function toast(message) {
  alert(message);
}

const URL_RE = /\bhttps?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]]/g;

// Text with its web links made clickable, as nodes and strings to append.
export function linkify(text) {
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

// An attachment that's there to show. kind is one of image, sticker, video, audio, voice or file.
export function attachmentNode({ url, kind, contentType, filename, size, width, height, caption }) {
  const type = contentType || "";
  let media;
  if (kind === "image" || kind === "sticker" || (type.startsWith("image/") && kind !== "file")) {
    media = el("a", { href: url, target: "_blank", rel: "noopener" },
      el("img", { src: url, loading: "lazy", alt: filename || "image", width: width || null, height: height || null }));
  } else if (kind === "video" || type.startsWith("video/")) {
    media = el("video", { src: url, controls: true, preload: "metadata" });
  } else if (kind === "audio" || kind === "voice" || type.startsWith("audio/")) {
    media = el("audio", { src: url, controls: true, preload: "metadata" });
  } else {
    media = el("a", { class: "file", href: url, download: filename || "" },
      `📄 ${filename || "file"}${size ? ` (${formatSize(size)})` : ""}`);
  }
  return caption ? el("div", {}, media, el("div", {}, ...linkify(caption))) : media;
}

// An attachment that isn't (or is no longer) there.
export function missingAttachmentNode(removed, filename) {
  const label = removed ? "Media removed" : "Media not available";
  return el("div", { class: "missing" }, `${label}${filename ? ` (${filename})` : ""}`);
}

// The reactions under a message, grouped by emoji. Each reaction is { emoji, name, mine }.
export function reactionsNode(reactions) {
  const groups = new Map();
  for (const r of reactions) {
    if (!groups.has(r.emoji)) groups.set(r.emoji, []);
    groups.get(r.emoji).push(r);
  }
  return el("div", { class: "reactions" }, [...groups].map(([emoji, rs]) => el("span", {
    class: rs.some((r) => r.mine) ? "mine" : "", title: rs.map((r) => (r.mine ? "You" : r.name)).join(", "),
  }, emoji, rs.length > 1 ? ` ${rs.length}` : "")));
}

const ICONS = {
  more: '<svg viewBox="0 0 24 24" width="20" height="20"><circle cx="12" cy="5" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="12" cy="19" r="2"/></svg>',
  reply: '<svg viewBox="0 0 24 24" width="20" height="20"><path d="M10 9V5l-7 7 7 7v-4.1c5 0 8.5 1.6 11 5.1-1-5-4-10-11-11z"/></svg>',
  react: '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.8"><circle cx="12" cy="12" r="9"/><path d="M8.5 14.5c1.9 2 5.1 2 7 0" stroke-linecap="round"/><circle cx="9" cy="10" r="1.1" fill="currentColor" stroke="none"/><circle cx="15" cy="10" r="1.1" fill="currentColor" stroke="none"/></svg>',
};

// One of the icon buttons beside a bubble: "react", "reply" or "more".
export function toolButton(name, title, onclick) {
  const button = el("button", { type: "button", class: "tool", title, "aria-label": title, onclick });
  button.innerHTML = ICONS[name];
  return button;
}

// A popover menu of [label, action] items.
export function menu(items) {
  return el("div", { class: "menu" }, items.map(([label, action]) => el("button", {
    type: "button", onclick: () => { closePopover(); action(); },
  }, label)));
}

// A row of emoji to react with, marking the one already chosen. onpick(emoji) is called with null to remove it.
export function reactionPicker(emojis, chosen, onpick) {
  return el("div", { class: "reaction-picker" }, emojis.map((emoji) => el("button", {
    type: "button",
    class: chosen === emoji ? "mine" : null,
    title: chosen === emoji ? "Remove reaction" : `React ${emoji}`,
    onclick: () => {
      closePopover();
      onpick(chosen === emoji ? null : emoji);
    },
  }, emoji)));
}

// One popover at a time, above the button that opened it. It stays until something in it is chosen, or a click
// elsewhere, Escape or scrolling closes it; its message keeps showing its tools meanwhile.
let popover = null;

export function openPopover(anchor, owner, content) {
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

export function closePopover() {
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

// On touch screens there's no hover: tapping a message shows its tools.
export function tapToShowTools(node) {
  node.addEventListener("click", (e) => {
    if (window.matchMedia("(hover: none)").matches && !e.target.closest("a,button,audio,video,img,.quote")) {
      for (const other of document.querySelectorAll(".msg.show-tools")) if (other !== node) other.classList.remove("show-tools");
      node.classList.toggle("show-tools");
    }
  });
}
