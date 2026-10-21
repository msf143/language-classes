// Class catalog + sign-in: fetch → state → render → listen.
// One state object is the source of truth; render functions derive the screen from it.
// Shared helpers (t, h, api, renderStaticText) live in common.js.

const state = {
  uiLang: initialUiLang(),         // "en" or "es": language of the interface
  filters: { language: "", audience: "" },
  status: "loading",               // "loading" | "ready" | "error"
  sessions: [],
  error: null,                     // { requestId } when status is "error"

  testSite: false,                 // staging or dev (from GET /env): show the banner + test credit
  addingCredit: false,             // a test-credit request is on its way

  me: undefined,                   // undefined = not checked yet, null = signed out, else the account
                                   // plus balanceCents and learners (from GET /me)
  myBookings: [],                  // my upcoming bookings (GET /bookings); [] when signed out
  book: {
    open: false,                   // is the Book dialog showing?
    sessionId: null,               // which class
    learnerIds: [],                // who's ticked (copied from the checkboxes on every change)
    key: "",                       // Idempotency-Key for this attempt: new when opened or ticks change
    status: "idle",                // "idle" | "sending"
    error: null,                   // { key, requestId? } when the last attempt failed
  },
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
const bookDialogEl = document.querySelector("#book-dialog");
const bookFormEl = document.querySelector("#book-form");

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
    state.me = { ...res.data.account, balanceCents: res.data.balance_cents, learners: res.data.learners };
    loadMyBookings();   // signed in: fetch the "My bookings" list too
  } else {
    if (res.status !== 401) console.error("loadMe failed", res);   // 401 just means signed out
    state.me = null;
  }
  render();
}

// My upcoming bookings, for the "My bookings" list and the "You're booked"
// tags on cards. A failure just leaves the list as it was: the catalog still
// works, and the next reload tries again.
async function loadMyBookings() {
  const res = await api("GET", "/bookings");
  if (res.ok) {
    state.myBookings = res.data;
  } else if (res.status !== 401) {
    console.error("loadMyBookings failed", res);
  }
  render();
}

// Is this a test site (staging, dev)? If the call fails, assume production:
// better to hide the banner than to show it on the real site.
async function loadEnv() {
  const res = await api("GET", "/env");
  state.testSite = res.ok && res.data.test_site === true;
  render();
}

// Test sites only: +$50 of fake credit (the server refuses beyond $500).
async function addTestCredit() {
  state.addingCredit = true;
  render();
  const res = await api("POST", "/test-credit");
  state.addingCredit = false;
  if (res.ok) {
    state.me.balanceCents = res.data.balance_cents;
    showToast(t("testCreditAdded", money(res.data.balance_cents)));
  } else if (res.data.code === "test_credit_limit") {
    showToast(t("testCreditLimit"));
  } else {
    showToast(`${t("somethingWrong")} ${t("reference")}: ${res.requestId}`);
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
  state.myBookings = [];
  render();
}

// ---------- rendering ----------

function render() {
  renderStaticText();
  renderBanner();
  tzNoteEl.textContent = t("timesShown", visitorTimeZone);
  renderAccount();
  renderSignin();
  renderBook();
  renderFilters();
  renderStatus();
  renderMyBookings();
  renderList();
}

// The bookings to show: none unless someone is signed in, however state.me got
// cleared (sign-out, expired session).
function myBookings() {
  return state.me ? state.myBookings : [];
}

// "My bookings": one row per upcoming booking.
function renderMyBookings() {
  const bookings = myBookings();
  document.querySelector("#my-bookings").hidden = bookings.length === 0;
  document.querySelector("#my-bookings-list").replaceChildren(...bookings.map((b) => {
    const { dateText, timeText } = formatWhen(b.session);
    const names = b.attendees.map((a) => a.display_name).join(", ");
    return h("li", { class: "my-booking", "data-booking-id": b.id },
      h("span", { class: "my-booking-title" }, b.session.title ?? t("privateClass")),
      h("span", { class: "my-booking-detail" },
        `${dateText} · ${timeText} · ${t("minutes", b.session.duration_minutes)}`),
      h("span", { class: "my-booking-detail" }, t("forWhom", names)),
      h("span", { class: "my-booking-detail" }, t("paid", money(b.total_cents, b.currency))),
    );
  }));
}

// The test-site strip: hidden on production; the credit and the button only
// when someone is signed in.
function renderBanner() {
  document.querySelector("#test-banner").hidden = !state.testSite;
  if (!state.testSite) return;
  const signedIn = Boolean(state.me);
  const credit = document.querySelector("#test-banner-credit");
  credit.textContent = signedIn ? t("yourCredit", money(state.me.balanceCents)) : "";
  const btn = document.querySelector("#test-credit-btn");
  btn.hidden = !signedIn;
  btn.textContent = t("addTestCredit", money(5000));
  btn.disabled = state.addingCredit;
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
  const { dateText, timeText } = formatWhen(s);
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

  const booked = myBookings().some((b) => b.session.id === s.id);

  return h("li", { class: "card", "data-session-id": s.id },
    h("div", { class: "card-tags" },
      h("span", { class: `tag tag-${s.target_language}` }, languageTag),
      h("span", { class: "tag" }, audienceText),
      booked ? h("span", { class: "tag tag-booked" }, t("youreBooked")) : "",
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

// A session's date and start time in the visitor's language and time zone.
function formatWhen(s) {
  const start = new Date(s.starts_at);
  const dateText = new Intl.DateTimeFormat(state.uiLang, {
    weekday: "long", month: "long", day: "numeric",
  }).format(start).replace(/^./, (c) => c.toUpperCase());   // "sábado…" → "Sábado…"
  const timeText = new Intl.DateTimeFormat(state.uiLang, {
    hour: "numeric", minute: "2-digit",
  }).format(start);
  return { dateText, timeText };
}

// money(1500) → "$15.00" (English) or "15,00 US$" (Spanish). The API sends whole
// cents; dividing by 100 here is for display only, never for arithmetic.
function money(cents, currency = "USD") {
  return new Intl.NumberFormat(state.uiLang, { style: "currency", currency }).format(cents / 100);
}

let toastTimer;
function showToast(message) {
  toastEl.textContent = message;
  toastEl.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toastEl.hidden = true; }, 4000);
}

// ---------- book dialog ----------

// Can this learner attend this session? Same rule the server checks.
function canAttend(session, learner) {
  if (session.audience === "adults") return !learner.is_minor;
  if (session.audience === "kids") return learner.is_minor;
  return true;   // "all": anyone
}

// What booking n people into session s costs, or which message says why it can't
// be booked. The server decides for real; this is so the dialog can show it first.
function quote(s, n) {
  if (n === 0) return { problem: "chooseAttendees" };
  if (s.seats_left !== null && n > s.seats_left) return { problem: "notEnoughSeats" };
  const price = s.prices.find((p) => p.party_size === n);
  if (!price) return { problem: "noPriceForParty" };
  const each = price.price_per_person_cents;
  return { each, total: each * n, currency: price.currency };
}

// Open the dialog for a session. The checkboxes are built once, here, not on
// every render: rebuilding them would lose keyboard focus while someone ticks.
function openBook(sessionId) {
  const session = state.sessions.find((s) => s.id === sessionId);
  if (!session) return;
  const eligible = state.me.learners.filter((l) => canAttend(session, l));
  const preselect = eligible.length === 1 ? eligible[0].id : null;   // only one person? tick them

  document.querySelector("#book-learners").replaceChildren(...state.me.learners.map((l) => {
    const ok = canAttend(session, l);
    return h("label", { class: ok ? "attendee" : "attendee is-disabled" },
      h("input", {
        type: "checkbox", name: "learner", value: l.id,
        disabled: ok ? null : "", checked: l.id === preselect ? "" : null,
      }),
      l.is_minor ? `${l.display_name} (${t("child")})` : l.display_name,
    );
  }));

  state.book = {
    open: true, sessionId, learnerIds: preselect ? [preselect] : [],
    key: crypto.randomUUID(), status: "idle", error: null,
  };
  render();
}

// Which message to show for an error code from POST /bookings.
const bookingErrorKeys = {
  insufficient_credit: "errInsufficientCredit",
  session_full: "errSessionFull",
  session_not_open: "errSessionNotOpen",
  session_not_found: "errSessionNotOpen",
  already_booked: "errAlreadyBooked",
  audience_mismatch: "errAudience",
  no_price: "noPriceForParty",
};

// Send the booking. Pressing Book again after a failure resends the same
// Idempotency-Key, so if the first request did get through, the server answers
// with that booking instead of booking (and charging) a second time.
async function submitBooking() {
  const b = state.book;
  b.status = "sending";
  b.error = null;
  render();

  const res = await api("POST", "/bookings",
    { session_id: b.sessionId, learner_ids: b.learnerIds },
    { "Idempotency-Key": b.key });
  b.status = "idle";

  if (res.ok) {   // 201 booked now, or 200 = this attempt had already gone through
    const title = state.sessions.find((s) => s.id === b.sessionId)?.title ?? "";
    b.open = false;
    render();
    showToast(t("booked", title));
    loadMe();         // new balance
    loadSessions();   // new seat counts
    return;
  }
  if (res.status === 401) {   // signed out meanwhile (expired, or signed out in another tab)
    b.open = false;
    state.me = null;
    openSignin();
    state.signin.error = { key: "errSignedOut" };
    state.signin.status = "error";
    render();
    return;
  }
  const key = bookingErrorKeys[res.data.code];
  b.error = key ? { key } : { key: "somethingWrong", requestId: res.requestId };
  render();
  // The class or the balance may have changed since the page loaded (someone
  // took the last seat, say): reload both so the dialog shows the real numbers.
  if (key) { loadMe(); loadSessions(); }
}

// Fill in the dialog from state: which class, why some people are greyed out,
// the price for who's ticked, and the balance before and after.
function renderBook() {
  const b = state.book;
  if (b.open && !bookDialogEl.open) bookDialogEl.showModal();
  if (!b.open && bookDialogEl.open) bookDialogEl.close();
  if (!b.open) return;

  const s = state.sessions.find((x) => x.id === b.sessionId);
  if (!s) {   // the class vanished from the list on a reload (started or cancelled)
    b.open = false;
    bookDialogEl.close();
    return;
  }
  const { dateText, timeText } = formatWhen(s);

  const err = b.error;
  document.querySelector("#book-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
  document.querySelector("#book-title").textContent = s.title;
  document.querySelector("#book-when").textContent =
    `${dateText} · ${timeText} · ${t("minutes", s.duration_minutes)}`;

  const someoneExcluded = state.me.learners.some((l) => !canAttend(s, l));
  document.querySelector("#book-audience-note").textContent =
    !someoneExcluded ? "" : s.audience === "adults" ? t("adultsOnlyNote") : t("kidsOnlyNote");

  const q = quote(s, b.learnerIds.length);
  const priceEl = document.querySelector("#book-price");
  const balanceEl = document.querySelector("#book-balance");
  const submit = document.querySelector("#book-submit");
  const balance = state.me.balanceCents;

  if (q.problem) {
    priceEl.textContent = t(q.problem, s.seats_left);
    balanceEl.textContent = "";
    submit.textContent = t("book");
    submit.disabled = true;
    return;
  }
  const short = q.total > balance;
  priceEl.textContent = t("priceLine", b.learnerIds.length, money(q.each, q.currency), money(q.total, q.currency));
  balanceEl.textContent = short
    ? t("balanceShort", money(balance))
    : t("balanceAfter", money(balance), money(balance - q.total));
  balanceEl.classList.toggle("is-short", short);
  submit.textContent = b.status === "sending" ? t("booking") : t("bookFor", money(q.total, q.currency));
  submit.disabled = short || b.status === "sending";   // no double submits while one is on its way
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
  if (action === "book") {
    // Signed out: sign in first. Still checking (undefined): ignore the click.
    const sessionId = e.target.closest("[data-session-id]").dataset.sessionId;
    if (state.me === null) openSignin();
    else if (state.me) openBook(sessionId);
  }
  if (action === "close-book") { state.book.open = false; render(); }
  if (action === "open-signin") openSignin();
  if (action === "close-signin") { state.signin.open = false; render(); }
  if (action === "logout") logout();
  if (action === "test-credit" && !state.addingCredit) addTestCredit();
});

// "submit" bubbles like "click", so one document listener handles every form.
// preventDefault stops the browser's own submit (a full page load); we send
// the data with fetch instead. FormData reads the form's current input values.
document.addEventListener("submit", (e) => {
  if (e.target === bookFormEl) {
    e.preventDefault();
    if (state.book.status !== "sending") submitBooking();
    return;
  }
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

// Ticking or unticking a box in the Book dialog: copy who's ticked into state
// (FormData reads the checked boxes named "learner"), then re-render the price.
// "change" bubbles, so one document listener catches it.
document.addEventListener("change", (e) => {
  if (e.target.form !== bookFormEl) return;
  state.book.learnerIds = new FormData(bookFormEl).getAll("learner");
  state.book.key = crypto.randomUUID();   // different people = a different booking attempt
  state.book.error = null;
  render();
});

// Esc closes a <dialog> on its own; keep state in step when that happens.
// ("close" doesn't bubble, so these listeners sit on the dialogs themselves.)
dialogEl.addEventListener("close", () => { state.signin.open = false; });
bookDialogEl.addEventListener("close", () => { state.book.open = false; });

// signin.html links here as /#signin ("Request a new link"): open the dialog,
// then drop the #signin so a reload doesn't reopen it.
if (location.hash === "#signin") {
  history.replaceState(null, "", location.pathname);
  state.signin.open = true;
}

loadEnv();
loadMe();
loadSessions();
