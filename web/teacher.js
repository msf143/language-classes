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
};

const statusEl = document.querySelector("#status");
const addFormEl = document.querySelector("#add-block");

// ---------- data ----------

async function load() {
  const me = await api("GET", "/me");
  if (me.status === 401) { state.status = "signedOut"; return render(); }
  if (!me.ok) { state.status = "error"; state.error = { requestId: me.requestId }; return render(); }
  state.me = me.data.account;
  if (!state.me.is_teacher) { state.status = "notTeacher"; return render(); }
  await loadSchedule();
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
  renderBlocks();
  renderAddForm();
  renderClasses();
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
      h("span", { class: "my-booking-title" },
        `${c.kind === "private" ? t("privateClass") : c.title ?? t("groupClass")} · ` +
        (c.target_language === "es" ? t("learnSpanish") : t("learnEnglish"))),
      h("span", { class: "my-booking-detail" },
        `${dayText(c.starts_at)} · ${timeText(c.starts_at)} – ${timeText(c.ends_at)}`),
      h("span", { class: "my-booking-detail" }, t("seatsOf", c.seats_taken, c.max_seats)),
      h("span", { class: "my-booking-detail" }, names.length ? t("forWhom", names.join(", ")) : t("noOneYet")),
      hasMinor ? h("span", { class: "minor-note" }, t("minorNote")) : "",
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
});

document.addEventListener("submit", (e) => {
  if (e.target !== addFormEl) return;
  e.preventDefault();
  if (!state.adding) publish();
});

render();
load();
