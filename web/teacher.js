// Teacher dashboard: availability (publish / withdraw) and upcoming classes.
// Same pattern as app.js: one state object → render() → one click listener.
// Times are shown in the teacher's own time zone (from his account), wherever
// his browser happens to be; the server converts the times he types.

const state = {
  uiLang: initialUiLang(),
  status: "loading",        // "loading" | "signedOut" | "notTeacher" | "error" | "ready"
  error: null,              // { requestId } when status is "error"
  me: null,                 // the account (with timezone)
  blocks: [],               // GET /teacher/availability
  classes: [],              // GET /teacher/sessions
  adding: false,            // a publish request is on its way
  addError: null,           // { key } when the last publish failed
  profile: null,            // GET /teacher/profile (incl. allowed_lengths, max_break_minutes)
  savingProfile: false,     // a PUT is on its way
  profileError: null,       // { key, requestId? } when the last save failed
  prices: null,             // GET /teacher/prices (prices valid now + the form's limits)
  savingPrices: false,
  pricesError: null,        // { key, args?, requestId? }
  creatingClass: false,     // a new-class request is on its way
  classError: null,         // { key, requestId? } when the last create failed
  cancelClass: { open: false, id: null, status: "idle", error: null },   // the confirm dialog
};

const statusEl = document.querySelector("#status");
const addFormEl = document.querySelector("#add-block");
const profileFormEl = document.querySelector("#profile-form");
const pricesFormEl = document.querySelector("#prices-form");
const newClassFormEl = document.querySelector("#new-class");
const cancelClassDialogEl = document.querySelector("#cancel-class-dialog");
const cancelClassFormEl = document.querySelector("#cancel-class-form");

// ---------- data ----------

async function load() {
  const me = await api("GET", "/me");
  if (me.status === 401) { state.status = "signedOut"; return render(); }
  if (!me.ok) { state.status = "error"; state.error = { requestId: me.requestId }; return render(); }
  state.me = me.data.account;
  if (!state.me.is_teacher) { state.status = "notTeacher"; return render(); }
  // The profile, the prices and the schedule load side by side.
  const [profile, prices] = await Promise.all([
    api("GET", "/teacher/profile"), api("GET", "/teacher/prices"), loadSchedule(),
  ]);
  const failed = [profile, prices].find((res) => !res.ok);
  if (failed) { state.status = "error"; state.error = { requestId: failed.requestId }; return render(); }
  state.profile = profile.data;
  state.prices = prices.data;
  fillProfileForm();
  fillPricesForm();
  fillNewClassForm();
  render();
}

// Both lists at once: the two requests run in parallel (Promise.all waits for both).
async function loadSchedule() {
  const [blocks, classes] = await Promise.all([
    api("GET", "/teacher/availability"),
    api("GET", "/teacher/sessions"),
  ]);
  if (!blocks.ok || !classes.ok) {
    state.status = "error";
    state.error = { requestId: (blocks.ok ? classes : blocks).requestId };
  } else {
    state.blocks = blocks.data;
    state.classes = classes.data;
    state.status = "ready";
  }
  render();
}

// Publish what the form says. FormData reads the inputs as typed (the form owns them).
async function publish() {
  const form = new FormData(addFormEl);
  const body = {
    date: form.get("date"), from: form.get("from"), to: form.get("to"),
    repeat_weeks: Number(form.get("repeat_weeks")),
  };
  if (!body.date || !body.from || !body.to) {
    state.addError = { key: "fillAll" };
    return render();
  }
  state.adding = true;
  state.addError = null;
  render();
  const res = await api("POST", "/teacher/availability", body);
  state.adding = false;
  if (res.ok) {
    showToast(t("published", res.data.length));
    addFormEl.reset();
    await loadSchedule();
    return;
  }
  state.addError =
    res.data.code === "block_overlap" ? { key: "errBlockOverlap" } :
    res.status === 400 ? { key: "errBlockInput" } :
    { key: "somethingWrong", requestId: res.requestId };
  render();
}

// ---------- profile ----------

// Put the saved profile into the form. Called when it loads and after a save,
// never from render(): re-rendering (e.g. switching language) must not wipe
// what the teacher is typing.
function fillProfileForm() {
  const p = state.profile;
  const f = profileFormEl.elements;   // the form's inputs, by name
  f.display_name.value = p.display_name;
  f.bio_en.value = p.bio_en;
  f.bio_es.value = p.bio_es;
  // Choices come from the server's limits: 0, 5, 10, 15 minutes; 30, 45, 60.
  const breaks = [];
  for (let m = 0; m <= p.max_break_minutes; m += 5) breaks.push(m);
  document.querySelector("#profile-break").replaceChildren(...breaks.map((m) =>
    h("option", { value: m, selected: m === p.break_minutes ? "" : null }, `${m} min`)));
  document.querySelector("#profile-lengths").replaceChildren(...p.allowed_lengths.map((len) =>
    h("label", { class: "attendee" },
      h("input", { type: "checkbox", name: "offered_lengths", value: len,
                   checked: p.offered_lengths.includes(len) ? "" : null }),
      `${len} min`)));
}

async function saveProfile() {
  const form = new FormData(profileFormEl);
  const body = {
    display_name: String(form.get("display_name")).trim(),
    bio_en: String(form.get("bio_en")),
    bio_es: String(form.get("bio_es")),
    break_minutes: Number(form.get("break_minutes")),
    offered_lengths: form.getAll("offered_lengths").map(Number),   // every ticked box
  };
  if (!body.display_name) {
    state.profileError = { key: "errProfileName" };
    return render();
  }
  state.savingProfile = true;
  state.profileError = null;
  render();
  const res = await api("PUT", "/teacher/profile", body);
  state.savingProfile = false;
  if (res.ok) {
    state.profile = res.data;   // what the server stored (trimmed, sorted)
    fillProfileForm();
    showToast(t("profileSaved"));
  } else {
    state.profileError = res.data.code === "invalid_profile"
      ? { key: "errProfileInvalid" }
      : { key: "somethingWrong", requestId: res.requestId };
  }
  render();
}

function renderProfile() {
  const err = state.profileError;
  document.querySelector("#profile-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
  const submit = document.querySelector("#profile-submit");
  submit.textContent = state.savingProfile ? t("saving") : t("saveProfile");
  submit.disabled = state.savingProfile;
}

// ---------- prices ----------

// Dollars as typed ↔ whole cents, without floating-point arithmetic (0.1 + 0.2
// isn't exactly 0.3 in binary floating point; money must be exact).
// "25" → 2500, "25.5" → 2550, "25,50" → 2550 (Spanish comma), "" → null (no price).
function textToCents(text) {
  const s = text.trim();
  if (s === "") return null;
  const m = /^\$?(\d{1,4})(?:[.,](\d{1,2}))?$/.exec(s);
  if (!m) return NaN;
  return Number(m[1]) * 100 + Number((m[2] ?? "0").padEnd(2, "0"));
}
const centsToText = (c) => `${Math.floor(c / 100)}.${String(c % 100).padStart(2, "0")}`;

function money(cents) {
  return new Intl.NumberFormat(state.uiLang, { style: "currency", currency: "USD" }).format(cents / 100);
}

// Build the table: a column per length; a "Group" row (one price per student,
// whatever the party size), then a row per private party size. Input names say
// which cell they are: "group-60", "private-60-2". Built once per load or
// save, like the profile form; renderPrices only updates the row labels.
function fillPricesForm() {
  const a = state.prices;
  const price = (kind, minutes, people) =>
    a.prices.find((p) => p.kind === kind && p.duration_minutes === minutes && p.party_size === people);
  const input = (name, p) => h("input", {
    type: "text", inputmode: "decimal", name, class: "price-input",
    value: p ? centsToText(p.price_per_person_cents) : "", "aria-label": name,
  });
  document.querySelector("#prices-head").replaceChildren(
    h("th", {}, ""), ...a.allowed_lengths.map((len) => h("th", { scope: "col" }, `${len} min`)));
  const rows = [h("tr", {},
    h("th", { scope: "row", "data-price-label": "group" }),
    ...a.allowed_lengths.map((len) => h("td", {}, input(`group-${len}`, price("group", len, 1)))))];
  for (let n = 1; n <= a.max_party_size; n++) {
    rows.push(h("tr", {},
      h("th", { scope: "row", "data-price-label": `private-${n}` }),
      ...a.allowed_lengths.map((len) => h("td", {}, input(`private-${len}-${n}`, price("private", len, n))))));
  }
  document.querySelector("#prices-body").replaceChildren(...rows);
}

async function savePrices() {
  const a = state.prices;
  const form = new FormData(pricesFormEl);
  const list = [];
  const add = (kind, minutes, people, text) => {
    const cents = textToCents(String(text));
    if (cents === null) return true;          // empty: no price
    if (Number.isNaN(cents)) return false;    // not a dollar amount
    list.push({ kind, duration_minutes: minutes, party_size: people, price_per_person_cents: cents });
    return true;
  };
  let ok = true;
  for (const len of a.allowed_lengths) {
    // The group price counts for every party size (a family of 3 pays it 3 times).
    for (let n = 1; n <= a.max_party_size; n++) ok = add("group", len, n, form.get(`group-${len}`)) && ok;
    for (let n = 1; n <= a.max_party_size; n++) ok = add("private", len, n, form.get(`private-${len}-${n}`)) && ok;
  }
  if (!ok) {
    state.pricesError = { key: "errPriceFormat" };
    return render();
  }
  state.savingPrices = true;
  state.pricesError = null;
  render();
  const res = await api("PUT", "/teacher/prices", { prices: list });
  state.savingPrices = false;
  if (res.ok) {
    state.prices = res.data;
    fillPricesForm();
    showToast(t("pricesSaved"));
  } else if (res.data.code === "invalid_prices") {
    state.pricesError = { key: "errPriceRange", args: [money(a.min_price_cents), money(a.max_price_cents)] };
  } else {
    state.pricesError = { key: "somethingWrong", requestId: res.requestId };
  }
  render();
}

function renderPrices() {
  for (const th of document.querySelectorAll("[data-price-label]")) {
    const label = th.dataset.priceLabel;
    th.textContent = label === "group" ? t("priceRowGroup") : t("priceRowPrivate", Number(label.split("-")[1]));
  }
  const err = state.pricesError;
  document.querySelector("#prices-error").textContent =
    err ? t(err.key, ...(err.args ?? [])) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
  const submit = document.querySelector("#prices-submit");
  submit.textContent = state.savingPrices ? t("saving") : t("savePrices");
  submit.disabled = state.savingPrices;
}

// ---------- group classes: create, cancel ----------

const classErrorKeys = {
  invalid_class: "errClassInvalid",
  too_soon: "errClassTooSoon",
  no_price: "errClassNoPrice",
  class_overlap: "errClassOverlap",
};

// The form's choices: lengths from the server (same as prices), languages and
// audiences from the fixed lists. Built once (choices don't change while the
// page is open); the labels follow the language on each render.
function fillNewClassForm() {
  document.querySelector("#new-class-length").replaceChildren(...state.prices.allowed_lengths.map((len) =>
    h("option", { value: len, selected: len === 60 ? "" : null }, `${len} min`)));
  document.querySelector("#new-class-language").replaceChildren(
    h("option", { value: "es", "data-label": "learnSpanish" }), h("option", { value: "en", "data-label": "learnEnglish" }));
  document.querySelector("#new-class-audience").replaceChildren(
    h("option", { value: "adults", "data-label": "adults" }), h("option", { value: "kids", "data-label": "kids" }),
    h("option", { value: "all", "data-label": "allAges" }));
}

async function createClass() {
  const form = new FormData(newClassFormEl);
  const maxSeats = String(form.get("max_seats")).trim();
  const body = {
    title: String(form.get("title")).trim(),
    target_language: form.get("target_language"),
    audience: form.get("audience"),
    date: form.get("date"),
    from: form.get("from"),
    duration_minutes: Number(form.get("duration_minutes")),
    min_seats: Number(form.get("min_seats")),
    max_seats: maxSeats === "" ? null : Number(maxSeats),   // empty = no cap
  };
  if (!body.title || !body.date || !body.from) {
    state.classError = { key: "errClassFill" };
    return render();
  }
  state.creatingClass = true;
  state.classError = null;
  render();
  const res = await api("POST", "/teacher/sessions", body);
  state.creatingClass = false;
  if (res.ok) {
    newClassFormEl.reset();
    showToast(t("classCreated"));
    await loadSchedule();
    return;
  }
  const key = classErrorKeys[res.data.code];
  state.classError = key ? { key } : { key: "somethingWrong", requestId: res.requestId };
  render();
}

function renderNewClassForm() {
  for (const opt of newClassFormEl.querySelectorAll("option[data-label]")) opt.textContent = t(opt.dataset.label);
  // The earliest day the server accepts: 2 days ahead (it checks the exact 48 hours).
  document.querySelector("#new-class-date").min = dayKey(new Date(Date.now() + 2 * 86400000).toISOString());
  const submit = document.querySelector("#new-class-submit");
  submit.textContent = state.creatingClass ? t("creating") : t("createClass");
  submit.disabled = state.creatingClass;
  const err = state.classError;
  document.querySelector("#new-class-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
}

function openCancelClass(id) {
  state.cancelClass = { open: true, id, status: "idle", error: null };
  render();
}

// The dialog says which class and what happens to the people booked.
function renderCancelClass() {
  const c = state.cancelClass;
  if (c.open && !cancelClassDialogEl.open) cancelClassDialogEl.showModal();
  if (!c.open && cancelClassDialogEl.open) cancelClassDialogEl.close();
  if (!c.open) return;
  const cls = state.classes.find((x) => x.id === c.id);
  if (!cls) { c.open = false; cancelClassDialogEl.close(); return; }
  document.querySelector("#cancel-class-what").textContent =
    `${classTitle(cls)} · ${dayText(cls.starts_at)} · ${timeText(cls.starts_at)}`;
  document.querySelector("#cancel-class-effect").textContent =
    cls.seats_taken > 0 ? t("cancelClassRefunds", cls.seats_taken) : t("cancelClassNobody");
  document.querySelector("#cancel-class-error").textContent =
    c.error ? t(c.error.key) + (c.error.requestId ? ` ${t("reference")}: ${c.error.requestId}` : "") : "";
  const submit = document.querySelector("#cancel-class-submit");
  submit.textContent = c.status === "sending" ? t("cancelling") : t("cancelClass");
  submit.disabled = c.status === "sending";
}

async function submitCancelClass() {
  const c = state.cancelClass;
  c.status = "sending";
  c.error = null;
  render();
  const res = await api("POST", `/teacher/sessions/${c.id}/cancel`);
  c.status = "idle";
  if (res.ok) {
    c.open = false;
    showToast(t("classCancelled", res.data.bookings_refunded));
    await loadSchedule();
    return;
  }
  c.error = res.data.code === "class_started" ? { key: "errClassStarted" } : { key: "somethingWrong", requestId: res.requestId };
  render();
}

function classTitle(c) {
  return `${c.kind === "private" ? t("privateClass") : c.title ?? t("groupClass")} · ` +
    (c.target_language === "es" ? t("learnSpanish") : t("learnEnglish"));
}

async function withdraw(blockId) {
  const res = await api("POST", `/teacher/availability/${blockId}/withdraw`);
  if (!res.ok) {
    showToast(`${t("somethingWrong")} ${t("reference")}: ${res.requestId}`);
    return;
  }
  showToast(t("withdrawn"));
  await loadSchedule();
}

// ---------- formatting, always in the teacher's time zone ----------

function tz() { return state.me?.timezone || "UTC"; }

function fmt(iso, options) {
  return new Intl.DateTimeFormat(state.uiLang, { ...options, timeZone: tz() }).format(new Date(iso));
}
const dayText = (iso) => fmt(iso, { weekday: "long", month: "long", day: "numeric" })
  .replace(/^./, (c) => c.toUpperCase());
const timeText = (iso) => fmt(iso, { hour: "numeric", minute: "2-digit" });

// "2026-10-27" for a moment, in the teacher's zone (the en-CA format happens to be year-month-day).
function dayKey(iso) {
  return new Intl.DateTimeFormat("en-CA", { timeZone: tz(), year: "numeric", month: "2-digit", day: "2-digit" })
    .format(new Date(iso));
}

// Group items by their day, keeping time order: [[dayISO, [items]], ...].
function byDay(items, startOf) {
  const groups = new Map();
  for (const item of items) {
    const key = dayKey(startOf(item));
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(item);
  }
  return [...groups.values()];
}

// ---------- rendering ----------

function render() {
  renderStaticText();
  document.querySelector("#tz-note").textContent = state.me?.is_teacher ? t("tzNoteTeacher", tz()) : "";
  renderStatus();
  document.querySelector("#dashboard").hidden = state.status !== "ready";
  if (state.status !== "ready") return;
  document.querySelector("#pending-note").hidden = state.profile?.status !== "pending";
  renderProfile();
  renderPrices();
  renderBlocks();
  renderAddForm();
  renderClasses();
  renderNewClassForm();
  renderCancelClass();
}

function renderStatus() {
  const messages = { loading: t("loading"), signedOut: t("pleaseSignIn"), notTeacher: t("notTeacher") };
  statusEl.hidden = state.status === "ready";
  statusEl.className = state.status === "error" ? "status is-error" : "status";
  statusEl.replaceChildren();
  if (state.status === "error") {
    statusEl.append(h("p", {}, t("loadError")), h("p", { class: "ref" }, `${t("reference")}: ${state.error.requestId}`));
  } else if (state.status === "signedOut") {
    statusEl.append(h("p", {}, messages.signedOut), h("a", { class: "link", href: "/#signin" }, t("signIn")));
  } else {
    statusEl.textContent = messages[state.status] ?? "";
  }
}

// One row per day: "Tuesday, October 27   9:00 AM – 12:00 PM [Withdraw]   6:00 PM – 9:00 PM [Withdraw]".
function renderBlocks() {
  const list = document.querySelector("#block-list");
  if (state.blocks.length === 0) {
    list.replaceChildren(h("li", { class: "muted" }, t("noAvailability")));
    return;
  }
  list.replaceChildren(...byDay(state.blocks, (b) => b.starts_at).map((blocks) =>
    h("li", { class: "day-row" },
      h("span", { class: "day-name" }, dayText(blocks[0].starts_at)),
      ...blocks.map((b) => h("span", { class: "time-chip", "data-block-id": b.id },
        `${timeText(b.starts_at)} – ${timeText(b.ends_at)}`,
        h("button", { type: "button", class: "chip-action", "data-action": "withdraw" }, t("withdraw")),
      )),
    )));
}

// The form's own inputs are never rebuilt (they keep what's typed); only the
// labels on the repeat options, the earliest date, the button and the error.
function renderAddForm() {
  const repeat = document.querySelector("#add-repeat");
  const chosen = repeat.value || "1";
  repeat.replaceChildren(...Array.from({ length: 12 }, (_, i) =>
    h("option", { value: String(i + 1) }, t("repeatWeeks", i + 1))));
  repeat.value = chosen;
  document.querySelector("#add-date").min = dayKey(new Date().toISOString());
  const submit = document.querySelector("#add-submit");
  submit.textContent = state.adding ? t("publishing") : t("publish");
  submit.disabled = state.adding;
  const err = state.addError;
  document.querySelector("#add-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
}

function renderClasses() {
  const list = document.querySelector("#class-list");
  if (state.classes.length === 0) {
    list.replaceChildren(h("li", { class: "muted" }, t("noClasses")));
    return;
  }
  list.replaceChildren(...state.classes.map((c) => {
    const names = c.attendees.map((a) => (a.is_minor ? `${a.display_name} (${t("child")})` : a.display_name));
    const hasMinor = c.attendees.some((a) => a.is_minor);
    return h("li", { class: "my-booking" },
      h("span", { class: "my-booking-title" }, classTitle(c)),
      h("span", { class: "my-booking-detail" },
        `${dayText(c.starts_at)} · ${timeText(c.starts_at)} – ${timeText(c.ends_at)}`),
      h("span", { class: "my-booking-detail" }, t("seatsOf", c.seats_taken, c.max_seats)),
      h("span", { class: "my-booking-detail" }, names.length ? t("forWhom", names.join(", ")) : t("noOneYet")),
      hasMinor ? h("span", { class: "minor-note" }, t("minorNote")) : "",
      // Only classes that haven't started can be cancelled.
      new Date(c.starts_at) > new Date()
        ? h("button", { type: "button", class: "btn-secondary btn-small", "data-action": "cancel-class", "data-class-id": c.id },
            t("cancelClass"))
        : "",
    );
  }));
}

let toastTimer;
function showToast(message) {
  const el = document.querySelector("#toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 4000);
}

// ---------- events ----------

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) return render();
  const action = e.target.closest("[data-action]")?.dataset.action;
  if (action === "withdraw") withdraw(e.target.closest("[data-block-id]").dataset.blockId);
  if (action === "cancel-class") openCancelClass(e.target.closest("[data-class-id]").dataset.classId);
  if (action === "close-cancel-class") { state.cancelClass.open = false; render(); }
});

document.addEventListener("submit", (e) => {
  if (e.target === profileFormEl) {
    e.preventDefault();
    if (!state.savingProfile) saveProfile();
    return;
  }
  if (e.target === pricesFormEl) {
    e.preventDefault();
    if (!state.savingPrices) savePrices();
    return;
  }
  if (e.target === newClassFormEl) {
    e.preventDefault();
    if (!state.creatingClass) createClass();
    return;
  }
  if (e.target === cancelClassFormEl) {
    e.preventDefault();
    if (state.cancelClass.status !== "sending") submitCancelClass();
    return;
  }
  if (e.target !== addFormEl) return;
  e.preventDefault();
  if (!state.adding) publish();
});

render();
load();

// Esc closes a <dialog> on its own; keep state in step ("close" doesn't bubble).
cancelClassDialogEl.addEventListener("close", () => { state.cancelClass.open = false; });
