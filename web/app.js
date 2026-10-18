// Class catalog + sign-in: fetch → state → render → listen.
// One state object is the source of truth; render functions derive the screen from it.
// Shared helpers (t, h, api, renderStaticText) live in common.js.

const state = {
  uiLang: initialUiLang(),         // "en" or "es": language of the interface
  filters: { language: "", audience: "" },
  status: "loading",               // "loading" | "ready" | "error"
  sessions: [],
  error: null,                     // { requestId } when status is "error"

  me: undefined,                   // undefined = not checked yet, null = signed out, else the account
  signin: {
    open: false,                   // is the sign-in dialog showing?
    status: "idle",                // "idle" | "sending" | "sent" | "error"
    email: "",                     // where the link went (for the "sent" message)
    expiresInMinutes: 0,
    error: null,                   // { key, requestId? } when status is "error"
  },
};

const listEl = document.querySelector("#session-list");
const statusEl = document.querySelector("#status");
const tzNoteEl = document.querySelector("#tz-note");
const toastEl = document.querySelector("#toast");
const accountEl = document.querySelector("#account-area");
const dialogEl = document.querySelector("#signin-dialog");
const signinFormEl = document.querySelector("#signin-form");

// The visitor's time zone, e.g. "America/New_York". The API sends UTC; we convert here.
const visitorTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;

// ---------- data ----------

async function loadSessions() {
  state.status = "loading";
  render();

  // The public catalog lists group classes only. Private sessions belong to
  // whoever booked them; students will book privates from open time slots later.
  const params = new URLSearchParams({ kind: "group" });
  if (state.filters.language) params.set("language", state.filters.language);
  if (state.filters.audience) params.set("audience", state.filters.audience);

  const res = await api("GET", `/sessions?${params}`);
  if (res.ok) {
    state.sessions = res.data;
    state.status = "ready";
  } else {
    console.error("loadSessions failed", res);
    state.status = "error";
    state.error = { requestId: res.requestId };
  }
  render();
}

// Who is signed in? The session cookie is HttpOnly, so JavaScript can't see
// it; the browser sends it with the request and the server tells us.
async function loadMe() {
  const res = await api("GET", "/me");
  if (res.ok) {
    state.me = res.data.account;
  } else {
    if (res.status !== 401) console.error("loadMe failed", res);   // 401 just means signed out
    state.me = null;
  }
  render();
}

async function requestLink(email) {
  state.signin.status = "sending";
  state.signin.error = null;
  render();

  const res = await api("POST", "/auth/login", { email });
  if (res.ok) {
    state.signin.status = "sent";
    state.signin.email = email;
    state.signin.expiresInMinutes = res.data.expires_in_minutes;
    signinFormEl.reset();
  } else {
    state.signin.status = "error";
    state.signin.error =
      res.status === 400 ? { key: "invalidEmail" } :
      res.status === 429 ? { key: "tooManyLinks" } :
      { key: "somethingWrong", requestId: res.requestId };
  }
  render();
}

async function logout() {
  const res = await api("POST", "/auth/logout");
  if (!res.ok) {
    showToast(`${t("signoutFailed")} ${t("reference")}: ${res.requestId}`);
    return;
  }
  state.me = null;
  render();
}

// ---------- rendering ----------

function render() {
  renderStaticText();
  tzNoteEl.textContent = t("timesShown", visitorTimeZone);
  renderAccount();
  renderSignin();
  renderFilters();
  renderStatus();
  renderList();
}

// Header: nothing until we know, then "Sign in" or name + "Sign out".
function renderAccount() {
  accountEl.replaceChildren();
  if (state.me === undefined) return;   // avoids flashing "Sign in" for signed-in visitors
  if (state.me === null) {
    accountEl.append(h("button", { type: "button", class: "btn-primary btn-small", "data-action": "open-signin" }, t("signIn")));
    return;
  }
  accountEl.append(h("span", { class: "account-name", title: state.me.email }, state.me.display_name));
  if (state.me.is_teacher) accountEl.append(h("span", { class: "tag" }, t("teacherBadge")));
  accountEl.append(h("button", { type: "button", class: "btn-secondary btn-small", "data-action": "logout" }, t("signOut")));
}

// The dialog's markup is static in index.html; here we only open/close it and
// switch between the form and the "check your email" view. The <input> is never
// re-rendered, so whatever the visitor typed stays put (the form owns its input).
function renderSignin() {
  const s = state.signin;
  if (s.open && !dialogEl.open) dialogEl.showModal();
  if (!s.open && dialogEl.open) dialogEl.close();

  const sent = s.status === "sent";
  signinFormEl.hidden = sent;
  document.querySelector("#signin-sent").hidden = !sent;

  const submit = document.querySelector("#signin-submit");
  submit.textContent = s.status === "sending" ? t("sending") : t("sendLink");
  submit.disabled = s.status === "sending";

  const err = s.error;
  document.querySelector("#signin-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";

  if (sent) {
    document.querySelector("#signin-sent-text").textContent = t("signinSent", s.email);
    document.querySelector("#signin-sent-hint").textContent = t("signinSentHint", s.expiresInMinutes);
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

let toastTimer;
function showToast(message) {
  toastEl.textContent = message;
  toastEl.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toastEl.hidden = true; }, 4000);
}

function openSignin() {
  state.signin = { open: true, status: "idle", email: "", expiresInMinutes: 0, error: null };
  render();
}

// ---------- events (delegated listeners for the whole page) ----------

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) {
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
  if (action === "open-signin") openSignin();
  if (action === "close-signin") { state.signin.open = false; render(); }
  if (action === "logout") logout();
});

// "submit" bubbles like "click", so one document listener handles every form.
// preventDefault stops the browser's own submit (a full page load); we send
// the data with fetch instead. FormData reads the form's current input values.
document.addEventListener("submit", (e) => {
  if (e.target !== signinFormEl) return;
  e.preventDefault();
  const email = String(new FormData(signinFormEl).get("email")).trim();
  if (!email) {
    state.signin.status = "error";
    state.signin.error = { key: "invalidEmail" };
    render();
    return;
  }
  requestLink(email);
});

// Esc closes a <dialog> on its own; keep state in step when that happens.
// ("close" doesn't bubble, so this listener sits on the dialog itself.)
dialogEl.addEventListener("close", () => { state.signin.open = false; });

// signin.html links here as /#signin ("Request a new link"): open the dialog,
// then drop the #signin so a reload doesn't reopen it.
if (location.hash === "#signin") {
  history.replaceState(null, "", location.pathname);
  state.signin.open = true;
}

loadMe();
loadSessions();
