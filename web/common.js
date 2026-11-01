// Helpers shared by every page (index.html, signin.html).
// Each page's own script defines a global `state` object with a `uiLang` field;
// t() and renderStaticText() read it.

// t("key") or t("key", arg): look up interface text in the current language.
function t(key, ...args) {
  const value = STRINGS[state.uiLang][key];
  return typeof value === "function" ? value(...args) : value;
}

function initialUiLang() {
  try {
    const saved = localStorage.getItem("uiLang");
    if (saved === "en" || saved === "es") return saved;
  } catch { /* storage can be blocked; fall through */ }
  return navigator.language.toLowerCase().startsWith("es") ? "es" : "en";
}

// If the click was on an EN/ES button, switch language and return true.
// The page then re-renders; text changes never need a refetch.
function handleLangClick(e) {
  const btn = e.target.closest("[data-ui-lang]");
  if (!btn) return false;
  state.uiLang = btn.dataset.uiLang;
  try { localStorage.setItem("uiLang", state.uiLang); } catch { /* ignore */ }
  return true;
}

// Fill every element marked data-i18n="key" with text in the current language,
// and highlight the active EN/ES button.
function renderStaticText() {
  document.documentElement.lang = state.uiLang;
  for (const el of document.querySelectorAll("[data-i18n]")) {
    el.textContent = t(el.dataset.i18n);
  }
  for (const btn of document.querySelectorAll("[data-ui-lang]")) {
    btn.setAttribute("aria-pressed", btn.dataset.uiLang === state.uiLang);
  }
}

// api(method, path, body?, headers?) calls our server and never throws. It
// resolves to { ok, status, data, requestId }; status is 0 if the network failed.
// Each call sends its own X-Request-ID, so a browser error and the server's
// log line share an ID ("Reference" in error messages). headers adds more,
// e.g. { "Idempotency-Key": "..." }.
async function api(method, path, body, headers = {}) {
  const requestId = crypto.randomUUID().replaceAll("-", "").slice(0, 16);
  const options = { method, headers: { ...headers, "X-Request-ID": requestId } };
  if (body instanceof Blob) {         // a file (a photo) goes as it is
    options.headers["Content-Type"] = body.type;
    options.body = body;
  } else if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  try {
    const res = await fetch(path, options);
    const data = await res.json().catch(() => ({}));
    return {
      ok: res.ok,
      status: res.status,
      data,
      requestId: data.request_id || res.headers.get("X-Request-ID") || requestId,
    };
  } catch (err) {
    console.error(`${method} ${path} failed`, { requestId, err });
    return { ok: false, status: 0, data: {}, requestId };
  }
}

// h("tag", {attributes}, ...children): a tiny helper for building elements.
// Attributes set to null are skipped; children can be strings or elements.
// Text goes in as text nodes, never parsed as HTML, so data can't inject markup.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs)) {
    if (value !== null && value !== undefined) el.setAttribute(name, value);
  }
  el.append(...children);
  return el;
}

// Pieces of a line joined by " · ", each kept whole ("1:00 p. m.", "15,00 US$"),
// so a narrow screen wraps between pieces, never inside one.
function dotted(pieces) {
  return pieces.flatMap((p, i) => [i ? " · " : "", h("span", { class: "nowrap" }, p)]);
}

// A card's top row: the title on the left, its action on the right.
function cardHead(title, action) {
  return h("div", { class: "card-head" }, h("h3", { class: "my-booking-title" }, ...title), action || "");
}

// ---------- profile photos ----------

const photoSide = 512;   // pixels; the square the server is sent
const photoMinSide = 64; // smaller than this is refused (the server's limit too)

// Turn the chosen file into a 512 × 512 JPEG: the middle square of the picture,
// shrunk, so a phone's several-MB photo uploads as about 50 KB. The browser does
// the reading, so whatever it can open works (usually including iPhone HEIC),
// and it applies the hidden "rotate me" tag phones add, so the result is upright.
// Throws an Error whose message is an i18n key.
async function photoToJpeg(file) {
  const url = URL.createObjectURL(file);
  try {
    const img = new Image();
    img.src = url;
    await img.decode().catch(() => { throw new Error("errPhotoUnreadable"); });
    const side = Math.min(img.naturalWidth, img.naturalHeight);
    if (side < photoMinSide) throw new Error("errPhotoTooSmall");
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = photoSide;
    const ctx = canvas.getContext("2d");
    ctx.fillStyle = "#ffffff";   // a transparent PNG gets white behind it, not JPEG's black
    ctx.fillRect(0, 0, photoSide, photoSide);
    ctx.imageSmoothingQuality = "high";
    ctx.drawImage(img, (img.naturalWidth - side) / 2, (img.naturalHeight - side) / 2, side, side, 0, 0, photoSide, photoSide);
    const blob = await new Promise((resolve) => canvas.toBlob(resolve, "image/jpeg", 0.85));
    if (!blob) throw new Error("errPhotoUnreadable");
    return blob;
  } finally {
    URL.revokeObjectURL(url);
  }
}

// Shrink and upload. Resolves to { photoId } or { error: { key, requestId? } }.
async function savePhoto(file) {
  let blob;
  try {
    blob = await photoToJpeg(file);
  } catch (err) {
    return { error: { key: err.message.startsWith("errPhoto") ? err.message : "errPhotoUnreadable" } };
  }
  const res = await api("PUT", "/me/photo", blob);
  if (res.ok) return { photoId: res.data.photo_id };
  return { error: { key: "errPhotoFailed", requestId: res.requestId } };
}

// The photo block (.photo-editor), the same on the teacher page and in My Account.
// A photo saves as soon as it's chosen. Each page has state.me, state.photoBusy,
// state.photoError, render() and showToast().

async function choosePhoto(file) {
  state.photoBusy = true;
  state.photoError = null;
  render();
  const result = await savePhoto(file);
  state.photoBusy = false;
  if (result.error) state.photoError = result.error;
  else {
    state.me.photo_id = result.photoId;
    showToast(t("photoSaved"));
  }
  render();
}

async function removePhoto() {
  if (!confirm(t("confirmRemovePhoto"))) return;
  state.photoBusy = true;
  state.photoError = null;
  render();
  const res = await api("POST", "/me/photo/remove");
  state.photoBusy = false;
  if (res.ok) {
    state.me.photo_id = null;
    showToast(t("photoRemoved"));
  } else {
    state.photoError = { key: "errPhotoFailed", requestId: res.requestId };
  }
  render();
}

function renderPhoto() {
  const id = state.me.photo_id;
  const image = document.querySelector("#photo-image");
  // Only when it changes: setting src again would flicker on every render.
  const src = id ? `/photos/${id}` : "";
  if (image.getAttribute("src") !== src) image.setAttribute("src", src);
  image.hidden = !id;
  image.alt = t("photoLabel");
  document.querySelector("#photo-empty").hidden = Boolean(id);
  document.querySelector("#photo-choose-text").textContent =
    state.photoBusy ? t("photoSaving") : id ? t("photoChange") : t("photoChoose");
  document.querySelector("#photo-input").disabled = state.photoBusy;
  const remove = document.querySelector("#photo-remove");
  remove.hidden = !id;
  remove.disabled = state.photoBusy;
  const err = state.photoError;
  document.querySelector("#photo-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
}

// An <img> for a photo, or nothing if there isn't one. The address never changes
// for one photo, so the browser redraws it from its cache without flicker.
function photoImg(id, alt, cssClass) {
  return id ? h("img", { class: cssClass, src: `/photos/${id}`, alt, width: "96", height: "96" }) : "";
}

// A photo chosen. Clearing the input lets the same file be chosen again later.
// (Scripts here run after the page is parsed; pages without the block skip this.)
document.querySelector("#photo-input")?.addEventListener("change", (e) => {
  const file = e.target.files[0];
  e.target.value = "";
  if (file) choosePhoto(file);
});

// ---------- passkeys (WebAuthn) ----------
// The browser's passkey functions speak raw bytes (ArrayBuffer); our server
// speaks base64url text (base64 with - and _ instead of + and /, no padding).
// These two convert between them.
function toB64(buffer) {
  const text = String.fromCharCode(...new Uint8Array(buffer));
  return btoa(text).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}
function fromB64(text) {
  const b64 = text.replaceAll("-", "+").replaceAll("_", "/");
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}

// Can this browser make passkeys the way our server reads them? getPublicKey()
// hands over the public key ready to use (Safari 16+, Chrome 85+, Firefox 119+).
const passkeysSupported = typeof window.PublicKeyCredential === "function" &&
  typeof AuthenticatorAttestationResponse === "function" &&
  "getPublicKey" in AuthenticatorAttestationResponse.prototype;

// A name for a new passkey, so the list in My Account can tell them apart:
// "iPhone · Safari", "Windows · Edge". Only a label; nothing depends on it.
function deviceName() {
  const ua = navigator.userAgent;
  const device = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android" :
    /Macintosh/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /CrOS/.test(ua) ? "Chromebook" :
    /Linux/.test(ua) ? "Linux" : "";
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\/|FxiOS/.test(ua) ? "Firefox" :
    /Chrome\/|CriOS/.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "";
  return [device, browser].filter(Boolean).join(" · ") || "Passkey";
}
