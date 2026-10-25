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
  if (body !== undefined) {
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
