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
