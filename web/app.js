// Class catalog + sign-in: fetch → state → render → listen.
// One state object is the source of truth; render functions derive the screen from it.
// Shared helpers (t, h, api, renderStaticText) live in common.js.

const state = {
  uiLang: initialUiLang(),         // "en" or "es": language of the interface
  filters: { language: "", audience: "" },
  status: "loading",               // "loading" | "ready" | "error"
  sessions: [],
  error: null,                     // { requestId } when status is "error"

  private: {                       // the "Private classes" section (GET /private-slots)
    language: "es",                // which language to learn: "es" or "en"
    minutes: 60,                   // class length: 30, 45 or 60
    status: "loading",             // "loading" | "ready" | "error"
    teachers: [],                  // [{ teacher_id, teacher_name, prices, slots }]
    error: null,                   // { requestId } when status is "error"
  },

  testSite: false,                 // staging or dev (from GET /env): show the banner + test credit
  addingCredit: false,             // a test-credit request is on its way
  payments: false,                 // can credit be bought here (GET /env)? Shows the Buy credit buttons
  breakMinutes: null,              // the "may end early" number (GET /policies); null until known

  me: undefined,                   // undefined = not checked yet, null = signed out, else the account
                                   // plus balanceCents and learners (from GET /me)
  myBookings: [],                  // my upcoming bookings (GET /bookings); [] when signed out
  cancel: {
    open: false,                   // is the Cancel dialog showing?
    bookingId: null,
    status: "idle",                // "idle" | "sending"
    error: null,                   // { key, requestId? } when the last attempt failed
  },
  book: {
    open: false,                   // is the Book dialog showing?
    target: null,                  // what's being booked: { kind: "group", id } for a catalog class,
                                   // or a private slot { kind: "private", teacher_id, starts_at, ... }
    learnerIds: [],                // who's ticked (copied from the checkboxes on every change)
    key: "",                       // Idempotency-Key for this attempt: new when opened or ticks change
    status: "idle",                // "idle" | "sending"
    error: null,                   // { key, requestId? } when the last attempt failed
  },
  buy: {
    open: false,                   // is the Buy credit dialog showing?
    status: "idle",                // "loading" | "ready" | "error" | "sending" | "redirecting"
    packs: [],                     // [{ id, price_cents, bonus_cents, credit_cents, currency }]
    packId: null,                  // the ticked pack
    key: null,                     // Idempotency-Key for this purchase attempt
    error: null,                   // { key, requestId? } when the last attempt failed
  },
  paid: null,                      // back from Stripe: { id?, status, creditCents?, balanceCents?, currency? };
                                   // status "checking" | "done" | "failed" | "slow" | "cancelled" | "signin"
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
const cancelDialogEl = document.querySelector("#cancel-dialog");
const cancelFormEl = document.querySelector("#cancel-form");
const buyDialogEl = document.querySelector("#buy-dialog");
const buyFormEl = document.querySelector("#buy-form");

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

// Free private-class times for the chosen length (the next 14 days). Public,
// like the catalog: anyone can browse; booking needs a sign-in.
async function loadPrivateSlots() {
  state.private.status = "loading";
  render();
  const res = await api("GET", `/private-slots?duration=${state.private.minutes}`);
  if (res.ok) {
    state.private.teachers = res.data;
    state.private.status = "ready";
  } else {
    console.error("loadPrivateSlots failed", res);
    state.private.status = "error";
    state.private.error = { requestId: res.requestId };
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
  state.payments = res.ok && res.data.payments === true;
  render();
}

// The class-break number for the "may end early" sentence: from GET /policies,
// the same number the policies page states. If it fails, the sentence stays hidden.
async function loadPolicies() {
  const res = await api("GET", "/policies");
  if (res.ok) state.breakMinutes = res.data.break_minutes;
  render();
}

// Test sites only: take over the seed teacher profile, then the header shows
// the "Teacher page" link (loadMe brings back is_teacher = true).
async function becomeTestTeacher() {
  const res = await api("POST", "/test-teacher");
  if (!res.ok) {
    showToast(`${t("somethingWrong")} ${t("reference")}: ${res.requestId}`);
    return;
  }
  showToast(t("testTeacherDone"));
  loadMe();
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
  renderPaid();
  // The break sentence (in the Private classes section and the Book dialog);
  // hidden until the number is known, and when no teacher takes a break.
  for (const el of document.querySelectorAll(".break-note")) {
    el.hidden = !state.breakMinutes;
    el.textContent = state.breakMinutes ? t("breakNote", state.breakMinutes) : "";
  }
  tzNoteEl.textContent = t("timesShown", visitorTimeZone);
  renderAccount();
  renderSignin();
  renderBook();
  renderCancel();
  renderBuy();
  renderFilters();
  renderStatus();
  renderMyBookings();
  renderList();
  renderPrivate();
}

// "Private classes": per teacher, a price line, then one row per day with a
// button per free start time, all in the visitor's time zone (like the catalog).
function renderPrivate() {
  const p = state.private;
  for (const group of document.querySelectorAll("[data-private]")) {
    const active = String(p[group.dataset.private]);
    for (const btn of group.querySelectorAll("button")) {
      btn.setAttribute("aria-pressed", btn.dataset.value === active);
    }
  }

  const statusEl = document.querySelector("#private-status");
  const teachers = p.teachers.filter((tr) => tr.slots.length > 0);
  statusEl.hidden = p.status === "ready" && teachers.length > 0;
  statusEl.className = p.status === "error" ? "status is-error" : "status";
  statusEl.textContent =
    p.status === "loading" ? t("loadingTimes") :
    p.status === "error" ? `${t("loadError")} ${t("reference")}: ${p.error.requestId}` :
    t("noTimes");

  const rows = [];
  if (p.status === "ready") {
    for (const tr of teachers) {
      const prices = tr.prices.map((pr) => t("pricePeople", pr.party_size, money(pr.price_per_person_cents, pr.currency)));
      rows.push(h("li", { class: "private-teacher" },
        h("span", { class: "teacher-name" }, t("withTeacher", tr.teacher_name)),
        h("span", { class: "private-prices" }, t("pricePerPerson", prices.join(" · "))),
      ));
      for (const slots of slotsByDay(tr.slots)) {
        rows.push(h("li", { class: "day-row" },
          h("span", { class: "day-name" }, formatWhen(slots[0]).dateText),
          ...slots.map((s) => h("button", {
            type: "button", class: "slot-chip", "data-action": "pick-slot",
            "data-teacher-id": tr.teacher_id, "data-starts-at": s.starts_at,
          }, formatWhen(s).timeText)),
        ));
      }
    }
  }
  document.querySelector("#private-days").replaceChildren(...rows);
}

// Group slots by the visitor's local calendar day, keeping time order.
function slotsByDay(slots) {
  const days = new Map();
  for (const s of slots) {
    const key = new Date(s.starts_at).toDateString(); // local day, e.g. "Tue Oct 27 2026"
    if (!days.has(key)) days.set(key, []);
    days.get(key).push(s);
  }
  return [...days.values()];
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
      h("button", { type: "button", class: "btn-secondary btn-small", "data-action": "cancel-booking" },
        t("cancelBooking")),
    );
  }));
}

// The test-site strip: hidden on production; the buttons only when someone is
// signed in. (The credit itself is in the header, on every site.)
function renderBanner() {
  document.querySelector("#test-banner").hidden = !state.testSite;
  if (!state.testSite) return;
  const signedIn = Boolean(state.me);
  const btn = document.querySelector("#test-credit-btn");
  btn.hidden = !signedIn;
  btn.textContent = t("addTestCredit", money(5000));
  btn.disabled = state.addingCredit;
  // "Become the test teacher": for signed-in testers who aren't a teacher yet.
  const teacherBtn = document.querySelector("#test-teacher-btn");
  teacherBtn.hidden = !signedIn || state.me.is_teacher;
  teacherBtn.textContent = t("becomeTestTeacher");
}

// Header: nothing until we know, then "Sign in", or name, credit, "Buy credit"
// and "Sign out".
function renderAccount() {
  accountEl.replaceChildren();
  if (state.me === undefined) return;   // avoids flashing "Sign in" for signed-in visitors
  if (state.me === null) {
    accountEl.append(h("button", { type: "button", class: "btn-primary btn-small", "data-action": "open-signin" }, t("signIn")));
    return;
  }
  accountEl.append(h("span", { class: "account-name", title: state.me.email }, state.me.display_name));
  accountEl.append(h("span", { class: "account-credit" }, t("yourCredit", money(state.me.balanceCents))));
  if (state.payments) {
    accountEl.append(h("button", { type: "button", class: "btn-secondary btn-small", "data-action": "open-buy" }, t("buyCredit")));
  }
  if (state.me.is_teacher) accountEl.append(h("a", { class: "link", href: "teacher.html" }, t("teacherLink")));
  if (state.me.is_admin) accountEl.append(h("a", { class: "link", href: "admin.html" }, t("adminLink")));
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

// Open the dialog for a group class from the catalog.
function openBookGroup(sessionId) {
  if (state.sessions.some((s) => s.id === sessionId)) openBook({ kind: "group", id: sessionId });
}

// Open the dialog for a free private time. The target describes the class that
// booking will create, in the same shape the dialog uses for a group class:
// starts_at, duration, prices (the teacher's private prices), seats (a party
// of up to 4) and an audience ("all": any age; the class is yours).
function openBookPrivate(teacherId, startsAt) {
  const teacher = state.private.teachers.find((tr) => tr.teacher_id === teacherId);
  if (!teacher || !teacher.slots.some((sl) => sl.starts_at === startsAt)) return;
  openBook({
    kind: "private", teacher_id: teacherId, teacher_name: teacher.teacher_name,
    starts_at: startsAt, duration_minutes: state.private.minutes,
    target_language: state.private.language, audience: "all",
    prices: teacher.prices, seats_left: 4,
  });
}

// What the dialog shows for its target: a group class is re-read from the
// catalog every time (so live seat counts and prices show), a private slot is
// used as described. null when a group class has gone from the list.
function bookTarget() {
  const tg = state.book.target;
  if (tg?.kind === "group") return state.sessions.find((s) => s.id === tg.id) ?? null;
  return tg;
}

// Open the dialog. The checkboxes are built once, here, not on every render:
// rebuilding them would lose keyboard focus while someone ticks.
function openBook(target) {
  state.book.target = target;
  const session = bookTarget();
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
    open: true, target, learnerIds: preselect ? [preselect] : [],
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
  slot_taken: "errSlotTaken",
  slot_unavailable: "errSlotUnavailable",
};

// Send the booking. Pressing Book again after a failure resends the same
// Idempotency-Key, so if the first request did get through, the server answers
// with that booking instead of booking (and charging) a second time.
async function submitBooking() {
  const b = state.book;
  b.status = "sending";
  b.error = null;
  render();

  // A group class is booked by its ID; a private class by the time it should be
  // (the server creates the session).
  const tg = b.target;
  const body = tg.kind === "group"
    ? { session_id: tg.id, learner_ids: b.learnerIds }
    : {
        private: {
          teacher_id: tg.teacher_id, starts_at: tg.starts_at,
          duration_minutes: tg.duration_minutes, target_language: tg.target_language,
        },
        learner_ids: b.learnerIds,
      };
  const res = await api("POST", "/bookings", body, { "Idempotency-Key": b.key });
  b.status = "idle";

  if (res.ok) {   // 201 booked now, or 200 = this attempt had already gone through
    b.open = false;
    render();
    showToast(t("booked", targetTitle(tg)));
    loadMe();             // new balance and My bookings
    loadSessions();       // new seat counts
    loadPrivateSlots();   // a private booking takes its time off the list
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
  if (key) { loadMe(); loadSessions(); loadPrivateSlots(); }
}

// "Conversational Spanish – Beginners", or "Private class · Learn Spanish".
function targetTitle(tg) {
  if (tg.kind === "group") return state.sessions.find((s) => s.id === tg.id)?.title ?? "";
  return `${t("privateClass")} · ${tg.target_language === "es" ? t("learnSpanish") : t("learnEnglish")}`;
}

// Fill in the dialog from state: which class, why some people are greyed out,
// the price for who's ticked, and the balance before and after.
function renderBook() {
  const b = state.book;
  if (b.open && !bookDialogEl.open) bookDialogEl.showModal();
  if (!b.open && bookDialogEl.open) bookDialogEl.close();
  if (!b.open) return;

  const s = bookTarget();
  if (!s) {   // the class vanished from the list on a reload (started or cancelled)
    b.open = false;
    bookDialogEl.close();
    return;
  }
  const { dateText, timeText } = formatWhen(s);

  const err = b.error;
  document.querySelector("#book-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
  document.querySelector("#book-title").textContent = targetTitle(b.target);
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

  const buyEl = document.querySelector("#book-buy");
  if (q.problem) {
    buyEl.hidden = true;
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
  buyEl.hidden = !short || !state.payments;   // not enough credit: offer to buy some (where you can)
  submit.textContent = b.status === "sending" ? t("booking") : t("bookFor", money(q.total, q.currency));
  submit.disabled = short || b.status === "sending" || !stillFree(b.target);   // no double submits either
}

// A private time is still bookable if the latest slot list still has it (it
// drops out when someone else books it). A group class always counts as free
// here; its seats are checked by quote().
function stillFree(tg) {
  if (tg.kind !== "private" || state.private.status !== "ready") return true;
  return state.private.teachers.some((tr) => tr.teacher_id === tg.teacher_id &&
    tr.slots.some((sl) => sl.starts_at === tg.starts_at));
}

// ---------- cancel dialog ----------

function openCancel(bookingId) {
  state.cancel = { open: true, bookingId, status: "idle", error: null };
  render();
}

// Which booking, and how much comes back. The amount is the server's preview
// (refund_if_cancelled_cents from GET /bookings), so the page never applies the
// 24-hour rule itself.
function renderCancel() {
  const c = state.cancel;
  if (c.open && !cancelDialogEl.open) cancelDialogEl.showModal();
  if (!c.open && cancelDialogEl.open) cancelDialogEl.close();
  if (!c.open) return;

  const b = myBookings().find((x) => x.id === c.bookingId);
  if (!b) {   // gone from the list (cancelled elsewhere, or signed out)
    c.open = false;
    cancelDialogEl.close();
    return;
  }
  const { dateText, timeText } = formatWhen(b.session);
  document.querySelector("#cancel-what").textContent =
    `${b.session.title ?? t("privateClass")} · ${dateText} · ${timeText}`;

  const refund = money(b.refund_if_cancelled_cents, b.currency);
  document.querySelector("#cancel-refund").textContent =
    b.refund_if_cancelled_cents === b.total_cents ? t("refundFull", refund) : t("refundLate", refund);

  const err = c.error;
  document.querySelector("#cancel-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";

  const submit = document.querySelector("#cancel-submit");
  submit.textContent = c.status === "sending" ? t("cancelling") : t("cancelBooking");
  submit.disabled = c.status === "sending";
}

// Which message to show for an error code from POST /bookings/{id}/cancel.
const cancelErrorKeys = {
  too_late: "errTooLate",
  not_cancellable: "errNotCancellable",
  booking_not_found: "errNotCancellable",
};

// Send the cancellation. No idempotency key needed: cancelling a booking that's
// already cancelled just returns it (the server refunds only once).
async function submitCancel() {
  const c = state.cancel;
  c.status = "sending";
  c.error = null;
  render();

  const res = await api("POST", `/bookings/${c.bookingId}/cancel`);
  c.status = "idle";

  if (res.ok) {
    c.open = false;
    render();
    // The server's refund, not the preview: it's what actually went back.
    showToast(t("cancelled", money(res.data.refund_cents, res.data.currency)));
    loadMe();         // new balance and booking list
    loadSessions();   // the freed seat
    return;
  }
  if (res.status === 401) {
    c.open = false;
    state.me = null;
    openSignin();
    state.signin.error = { key: "errSignedOut" };
    state.signin.status = "error";
    render();
    return;
  }
  const key = cancelErrorKeys[res.data.code];
  c.error = key ? { key } : { key: "somethingWrong", requestId: res.requestId };
  render();
  if (key) loadMe();   // the list is out of date: refresh it
}

// ---------- buy credit dialog ----------

// Open the dialog and fetch the packs (they're data on the server, so the page
// never has prices of its own). One Idempotency-Key per purchase attempt: a new
// one each time the dialog opens or another pack is ticked.
async function openBuy() {
  state.buy = { open: true, status: "loading", packs: [], packId: null, key: crypto.randomUUID(), error: null };
  document.querySelector("#buy-packs").replaceChildren();
  render();

  const res = await api("GET", "/credit-packs");
  const b = state.buy;
  if (!res.ok) {
    console.error("load credit packs failed", res);
    b.status = "error";
    b.error = { key: "loadError", requestId: res.requestId };
    render();
    return;
  }
  b.packs = res.data;
  b.status = "ready";
  // The radio buttons are built once, here, like the Book dialog's checkboxes
  // (rebuilding them on every render would lose the tick and keyboard focus);
  // renderBuy fills in their text.
  document.querySelector("#buy-packs").replaceChildren(...b.packs.map((p) =>
    h("label", { class: "pack" },
      h("input", { type: "radio", name: "pack", value: p.id }),
      h("span", { class: "pack-line", "data-pack-id": p.id }),
      p.bonus_cents > 0 ? h("span", { class: "tag tag-booked pack-bonus", "data-bonus-id": p.id }) : "",
    )));
  render();
}

function renderBuy() {
  const b = state.buy;
  if (b.open && !buyDialogEl.open) buyDialogEl.showModal();
  if (!b.open && buyDialogEl.open) buyDialogEl.close();
  if (!b.open) return;

  for (const p of b.packs) {
    document.querySelector(`[data-pack-id="${p.id}"]`).textContent =
      t("packLine", money(p.price_cents, p.currency), money(p.credit_cents, p.currency));
    const bonus = document.querySelector(`[data-bonus-id="${p.id}"]`);
    if (bonus) bonus.textContent = t("packBonus", money(p.bonus_cents, p.currency));
  }
  document.querySelector("#buy-status").textContent =
    b.status === "loading" ? t("loadingPacks") :
    b.status === "redirecting" ? t("openingPayment") : "";

  const err = b.error;
  document.querySelector("#buy-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";

  const pack = b.packs.find((p) => p.id === b.packId);
  const submit = document.querySelector("#buy-submit");
  submit.textContent = pack ? t("payAmount", money(pack.price_cents, pack.currency)) : t("continueToPayment");
  submit.disabled = !pack || b.status !== "ready";   // also stops double submits
}

// Which message to show for an error code from POST /payments/checkout.
const buyErrorKeys = {
  payments_unavailable: "errPaymentsUnavailable",
  payment_provider_error: "errPaymentProvider",
  pack_unavailable: "errPackUnavailable",
  already_paid: "errAlreadyPaid",
};

// Ask the server for a Stripe Checkout page, then leave for it. Nothing is
// paid yet: the credit arrives when Stripe tells our server (the webhook).
// Retrying after a failure resends the same key, so the server reuses the same
// payment and the same Stripe page instead of starting a second purchase.
async function submitBuy() {
  const b = state.buy;
  b.status = "sending";
  b.error = null;
  render();

  const res = await api("POST", "/payments/checkout", { pack_id: b.packId }, { "Idempotency-Key": b.key });
  if (res.ok) {
    b.status = "redirecting";
    render();
    location.assign(res.data.checkout_url);   // like clicking a link: Back returns here
    return;
  }
  b.status = "ready";
  if (res.status === 401) {
    b.open = false;
    state.me = null;
    openSignin();
    state.signin.error = { key: "errSignedOut" };
    state.signin.status = "error";
    render();
    return;
  }
  const key = buyErrorKeys[res.data.code];
  b.error = key ? { key } : { key: "somethingWrong", requestId: res.requestId };
  render();
}

// ---------- coming back from Stripe ----------

// Stripe sends the student back to /?paid=<payment id> after paying, or to
// /?paid=cancelled. The credit arrives separately, when Stripe's webhook reaches
// our server: usually within seconds, sometimes before the student is back. So
// we ask the server about the payment until it's settled. This only updates the
// screen: the money is decided on the server either way.
const paidPollMs = (attempt) => (attempt < 10 ? 1000 : 3000);   // 10 × 1 s, then every 3 s
const paidMaxAttempts = 30;                                         // about a minute in all

async function watchPayment(id) {
  for (let attempt = 0; attempt < paidMaxAttempts; attempt++) {
    const res = await api("GET", `/payments/${encodeURIComponent(id)}`);
    if (state.paid?.id !== id) return;   // the notice was closed: stop asking
    if (res.ok && res.data.status === "succeeded") {
      const d = res.data;
      state.paid = { id, status: "done", creditCents: d.credit_cents, balanceCents: d.balance_cents, currency: d.currency };
      loadMe();   // the header's credit (loadMe renders)
      return;
    }
    if (res.ok && res.data.status === "failed") { state.paid = { id, status: "failed" }; render(); return; }
    if (res.status === 401) { state.paid = { id, status: "signin" }; render(); return; }
    if (res.status === 400 || res.status === 404) { state.paid = null; render(); return; }   // not yours: nothing to show
    // Still pending (or the network or server hiccuped): wait, then ask again.
    await new Promise((resolve) => setTimeout(resolve, paidPollMs(attempt)));
  }
  if (state.paid?.id === id) { state.paid.status = "slow"; render(); }
}

function renderPaid() {
  const p = state.paid;
  const el = document.querySelector("#paid-notice");
  el.hidden = !p;
  if (!p) return;
  el.classList.toggle("is-error", p.status === "failed");
  document.querySelector("#paid-text").textContent =
    p.status === "done" ? t("paymentDone", money(p.creditCents, p.currency), money(p.balanceCents, p.currency)) :
    p.status === "failed" ? t("paymentFailed") :
    p.status === "slow" ? t("paymentSlow") :
    p.status === "cancelled" ? t("paymentCancelled") :
    p.status === "signin" ? t("paymentSignIn") :
    t("paymentChecking");
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

  const privateBtn = e.target.closest("[data-private] button");
  if (privateBtn) {
    const key = privateBtn.closest("[data-private]").dataset.private;
    const value = key === "minutes" ? Number(privateBtn.dataset.value) : privateBtn.dataset.value;
    if (state.private[key] === value) return;
    state.private[key] = value;
    // A new length means new times; the language doesn't change which times are free.
    if (key === "minutes") loadPrivateSlots(); else render();
    return;
  }

  const action = e.target.closest("[data-action]")?.dataset.action;
  if (action === "retry") loadSessions();
  if (action === "book") {
    // Signed out: sign in first. Still checking (undefined): ignore the click.
    const sessionId = e.target.closest("[data-session-id]").dataset.sessionId;
    if (state.me === null) openSignin();
    else if (state.me) openBookGroup(sessionId);
  }
  if (action === "close-book") { state.book.open = false; render(); }
  if (action === "pick-slot") {
    // A free private time: same as Book on a card (sign in first if needed).
    const chip = e.target.closest("[data-starts-at]");
    if (state.me === null) openSignin();
    else if (state.me) openBookPrivate(chip.dataset.teacherId, chip.dataset.startsAt);
  }
  if (action === "cancel-booking") {
    openCancel(e.target.closest("[data-booking-id]").dataset.bookingId);
  }
  if (action === "close-cancel") { state.cancel.open = false; render(); }
  if (action === "open-signin") openSignin();
  if (action === "close-signin") { state.signin.open = false; render(); }
  if (action === "open-buy" && state.me) {
    state.book.open = false;   // from the Book dialog: one dialog at a time
    openBuy();
  }
  if (action === "close-buy") { state.buy.open = false; render(); }
  if (action === "dismiss-paid") { state.paid = null; render(); }
  if (action === "logout") logout();
  if (action === "test-credit" && !state.addingCredit) addTestCredit();
  if (action === "test-teacher") becomeTestTeacher();
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
  if (e.target === cancelFormEl) {
    e.preventDefault();
    if (state.cancel.status !== "sending") submitCancel();
    return;
  }
  if (e.target === buyFormEl) {
    e.preventDefault();
    if (state.buy.status === "ready" && state.buy.packId) submitBuy();
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
  if (e.target.form === buyFormEl) {   // another pack = another purchase attempt
    state.buy.packId = new FormData(buyFormEl).get("pack");
    state.buy.key = crypto.randomUUID();
    state.buy.error = null;
    render();
    return;
  }
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
cancelDialogEl.addEventListener("close", () => { state.cancel.open = false; });
buyDialogEl.addEventListener("close", () => { state.buy.open = false; });

// Back from Stripe's page without paying: the browser may show this page from
// its memory (the "back-forward cache"), frozen as it was when we left, with
// the dialog still saying "Opening the payment page…". Make it usable again.
// The key stays the same, so Continue reopens the same Stripe page.
window.addEventListener("pageshow", (e) => {
  if (e.persisted && state.buy.status === "redirecting") {
    state.buy.status = "ready";
    render();
  }
});

// signin.html links here as /#signin ("Request a new link"): open the dialog,
// then drop the #signin so a reload doesn't reopen it.
if (location.hash === "#signin") {
  history.replaceState(null, "", location.pathname);
  state.signin.open = true;
}

// Back from Stripe: read ?paid=..., then drop it from the address bar, so a
// reload or a bookmark doesn't show the notice again.
const paidParam = new URLSearchParams(location.search).get("paid");
if (paidParam) {
  history.replaceState(null, "", location.pathname);
  if (paidParam === "cancelled") {
    state.paid = { status: "cancelled" };
  } else {
    state.paid = { id: paidParam, status: "checking" };
    watchPayment(paidParam);
  }
}

loadEnv();
loadPolicies();
loadMe();
loadSessions();
loadPrivateSlots();
