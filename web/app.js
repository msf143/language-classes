// Class catalog: fetch → state → render → listen.
// One state object is the source of truth; render functions derive the screen from it.

const state = {
  uiLang: initialUiLang(),         // "en" or "es": language of the interface
  filters: { language: "", audience: "" },
  status: "loading",               // "loading" | "ready" | "error"
  sessions: [],
  error: null,                     // { requestId } when status is "error"
};

const listEl = document.querySelector("#session-list");
const statusEl = document.querySelector("#status");
const tzNoteEl = document.querySelector("#tz-note");
const toastEl = document.querySelector("#toast");

// The visitor's time zone, e.g. "America/New_York". The API sends UTC; we convert here.
const visitorTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;

// ---------- text helpers ----------

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

// ---------- data ----------

async function loadSessions() {
  state.status = "loading";
  render();

  // The public catalog lists group classes only. Private sessions belong to
  // whoever booked them; students will book privates from open time slots later.
  const params = new URLSearchParams({ kind: "group" });
  if (state.filters.language) params.set("language", state.filters.language);
  if (state.filters.audience) params.set("audience", state.filters.audience);

  // Send our own request ID so a browser error and the server log line share it.
  const requestId = crypto.randomUUID().replaceAll("-", "").slice(0, 16);

  try {
    const res = await fetch(`/sessions?${params}`, { headers: { "X-Request-ID": requestId } });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw Object.assign(new Error(body.error || res.statusText), {
        requestId: body.request_id || res.headers.get("X-Request-ID") || requestId,
      });
    }
    state.sessions = await res.json();
    state.status = "ready";
  } catch (err) {
    console.error("loadSessions failed", { requestId: err.requestId || requestId, err });
    state.status = "error";
    state.error = { requestId: err.requestId || requestId };
  }
  render();
}

// ---------- rendering ----------

function render() {
  renderStaticText();
  renderFilters();
  renderStatus();
  renderList();
}

// Fill every element marked data-i18n="key" with text in the current language.
function renderStaticText() {
  document.documentElement.lang = state.uiLang;
  for (const el of document.querySelectorAll("[data-i18n]")) {
    el.textContent = t(el.dataset.i18n);
  }
  tzNoteEl.textContent = t("timesShown", visitorTimeZone);
  for (const btn of document.querySelectorAll("[data-ui-lang]")) {
    btn.setAttribute("aria-pressed", btn.dataset.uiLang === state.uiLang);
  }
}

// Highlight the active button in each filter group.
function renderFilters() {
  for (const group of document.querySelectorAll("[data-filter]")) {
    const active = state.filters[group.dataset.filter];
    for (const btn of group.querySelectorAll("button")) {
      btn.setAttribute("aria-pressed", btn.dataset.value === active);
    }
  }
}

function renderStatus() {
  statusEl.replaceChildren();
  statusEl.className = "status";

  if (state.status === "loading") {
    statusEl.textContent = t("loading");
  } else if (state.status === "error") {
    statusEl.classList.add("is-error");
    const retry = h("button", { type: "button", class: "btn-secondary", "data-action": "retry" }, t("retry"));
    statusEl.append(
      h("p", {}, t("loadError")),
      h("p", { class: "ref" }, `${t("reference")}: ${state.error.requestId}`),
      retry,
    );
  } else if (state.sessions.length === 0) {
    statusEl.textContent = t("empty");
  }
}

function renderList() {
  listEl.replaceChildren();
  if (state.status !== "ready") return;
  for (const session of state.sessions) {
    listEl.append(sessionCard(session));
  }
}

// Build one class card. Only textContent is used for data, never innerHTML,
// so nothing from the server can inject HTML into the page.
function sessionCard(s) {
  const start = new Date(s.starts_at);
  const dateText = new Intl.DateTimeFormat(state.uiLang, {
    weekday: "long", month: "long", day: "numeric",
  }).format(start).replace(/^./, (c) => c.toUpperCase());   // "sábado…" → "Sábado…"
  const timeText = new Intl.DateTimeFormat(state.uiLang, {
    hour: "numeric", minute: "2-digit",
  }).format(start);

  const title = s.title;
  const languageTag = s.target_language === "es" ? t("learnSpanish") : t("learnEnglish");
  const audienceText = { adults: t("adults"), kids: t("kids"), all: t("allAges") }[s.audience];

  let seatsText;
  let isFull = false;
  if (s.seats_left === null) {
    seatsText = t("openSeats");
  } else if (s.seats_left === 0) {
    seatsText = t("full");
    isFull = true;
  } else {
    seatsText = t("seatsLeft", s.seats_left);
  }

  return h("li", { class: "card", "data-session-id": s.id },
    h("div", { class: "card-tags" },
      h("span", { class: `tag tag-${s.target_language}` }, languageTag),
      h("span", { class: "tag" }, audienceText),
    ),
    h("h3", { class: "card-title" }, title),
    h("p", { class: "card-when" },
      h("span", { class: "card-date" }, dateText),
      h("span", { class: "card-time" }, `${timeText} · ${t("minutes", s.duration_minutes)}`),
    ),
    h("div", { class: "card-foot" },
      h("span", { class: isFull ? "seats is-full" : "seats" }, seatsText),
      h("button", {
        type: "button", class: "btn-primary", "data-action": "book",
        disabled: isFull ? "" : null,
      }, t("book")),
    ),
  );
}

// h("tag", {attributes}, ...children): a tiny helper for building elements.
// Attributes set to null are skipped; children can be strings or elements.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs)) {
    if (value !== null && value !== undefined) el.setAttribute(name, value);
  }
  el.append(...children);
  return el;
}

let toastTimer;
function showToast(message) {
  toastEl.textContent = message;
  toastEl.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toastEl.hidden = true; }, 2500);
}

// ---------- events (one delegated listener for the whole page) ----------

document.addEventListener("click", (e) => {
  const langBtn = e.target.closest("[data-ui-lang]");
  if (langBtn) {
    state.uiLang = langBtn.dataset.uiLang;
    try { localStorage.setItem("uiLang", state.uiLang); } catch { /* ignore */ }
    render();                       // text changes only, no need to refetch
    return;
  }

  const filterBtn = e.target.closest("[data-filter] button");
  if (filterBtn) {
    const key = filterBtn.closest("[data-filter]").dataset.filter;
    if (state.filters[key] === filterBtn.dataset.value) return;
    state.filters[key] = filterBtn.dataset.value;
    loadSessions();                 // filtering happens on the server
    return;
  }

  const action = e.target.closest("[data-action]")?.dataset.action;
  if (action === "retry") loadSessions();
  if (action === "book") showToast(t("bookingSoon"));   // real booking comes in step 3
});

loadSessions();
