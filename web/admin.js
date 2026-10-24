// Admin page: the teachers (list → one in full) and switching them on or off.
// Same pattern as the other pages: one state object → render() → one click
// listener. The server decides who may see this (requireAdmin); the page only
// shows what it's given.

const state = {
  uiLang: initialUiLang(),
  status: "loading",   // "loading" | "signedOut" | "notAdmin" | "error" | "ready"
  error: null,         // { requestId }
  teachers: [],        // GET /admin/teachers
  selected: null,      // GET /admin/teachers/{id} when one is open, else null
  busy: false,         // an activate/deactivate request is on its way
  refusal: null,       // { classes } when deactivating was refused (has_booked_classes)
  showInactive: false, // the list shows pending and active teachers; inactive ones on request
};

// ---------- data ----------

async function load() {
  const me = await api("GET", "/me");
  if (me.status === 401) { state.status = "signedOut"; return render(); }
  if (!me.ok) { state.status = "error"; state.error = { requestId: me.requestId }; return render(); }
  if (!me.data.account.is_admin) { state.status = "notAdmin"; return render(); }
  await loadTeachers();
}

async function loadTeachers() {
  const res = await api("GET", "/admin/teachers");
  if (!res.ok) { state.status = "error"; state.error = { requestId: res.requestId }; return render(); }
  state.teachers = res.data;
  state.status = "ready";
  render();
}

async function openTeacher(id) {
  const res = await api("GET", `/admin/teachers/${id}`);
  if (!res.ok) { showToast(`${t("somethingWrong")} ${t("reference")}: ${res.requestId}`); return; }
  state.selected = res.data;
  state.refusal = null;
  render();
  window.scrollTo(0, 0);
}

// Activate or deactivate the open teacher. A refusal (booked classes) is shown
// in the page with the classes in the way; anything else is a toast.
async function setStatus(active) {
  const id = state.selected.id;
  state.busy = true;
  state.refusal = null;
  render();
  const res = await api("POST", `/admin/teachers/${id}/${active ? "activate" : "deactivate"}`);
  state.busy = false;
  if (res.ok) {
    showToast(t(active ? "teacherActivated" : "teacherDeactivated"));
    await Promise.all([openTeacher(id), loadTeachers()]);
    return;
  }
  if (res.data.code === "has_booked_classes") {
    state.refusal = { classes: res.data.classes };
  } else {
    showToast(`${t("somethingWrong")} ${t("reference")}: ${res.requestId}`);
  }
  render();
}

// ---------- formatting (in the admin's own browser time zone) ----------

const fmt = (iso, options) => new Intl.DateTimeFormat(state.uiLang, options).format(new Date(iso));
const whenText = (c) =>
  `${fmt(c.starts_at, { weekday: "short", month: "short", day: "numeric" })} · ${fmt(c.starts_at, { hour: "numeric", minute: "2-digit" })}`;
const money = (cents) => new Intl.NumberFormat(state.uiLang, { style: "currency", currency: "USD" }).format(cents / 100);
const statusTag = (status) => h("span", { class: `tag tag-status-${status}` }, t(`status_${status}`));
const classTitle = (c) => (c.kind === "private" ? t("privateClass") : c.title ?? t("groupClass"));

// ---------- rendering ----------

function render() {
  renderStaticText();
  const statusEl = document.querySelector("#status");
  const messages = { loading: t("loading"), notAdmin: t("notAdmin") };
  statusEl.hidden = state.status === "ready";
  statusEl.className = state.status === "error" ? "status is-error" : "status";
  statusEl.replaceChildren();
  if (state.status === "error") {
    statusEl.append(h("p", {}, t("loadError")), h("p", { class: "ref" }, `${t("reference")}: ${state.error.requestId}`));
  } else if (state.status === "signedOut") {
    statusEl.append(h("p", {}, t("pleaseSignIn")), h("a", { class: "link", href: "/#signin" }, t("signIn")));
  } else {
    statusEl.textContent = messages[state.status] ?? "";
  }
  const ready = state.status === "ready";
  document.querySelector("#teacher-list-view").hidden = !ready || state.selected !== null;
  document.querySelector("#teacher-view").hidden = !ready || state.selected === null;
  if (!ready) return;
  if (state.selected) renderTeacher(); else renderList();
}

// One row per teacher: name, email, status, and what he has set up. Inactive
// teachers pile up over the years, so they're behind a toggle.
function renderList() {
  const list = document.querySelector("#teacher-list");
  const inactive = state.teachers.filter((tr) => tr.status === "inactive").length;
  const shown = state.teachers.filter((tr) => state.showInactive || tr.status !== "inactive");
  const toggle = inactive === 0 ? [] : [h("li", {},
    h("button", { type: "button", class: "link-button", "data-action": "toggle-inactive" },
      t(state.showInactive ? "hideInactive" : "showInactive", inactive)))];
  if (shown.length === 0) {
    list.replaceChildren(h("li", { class: "muted" }, t("noTeachers")), ...toggle);
    return;
  }
  list.replaceChildren(...toggle, ...shown.map((tr) =>
    h("li", { class: "my-booking", "data-teacher-id": tr.id },
      h("span", { class: "my-booking-title" }, tr.display_name, " ", statusTag(tr.status)),
      h("span", { class: "my-booking-detail" }, tr.email),
      h("span", { class: "my-booking-detail" }, t("teacherCounts", tr.upcoming_classes, tr.booked_classes, tr.prices)),
      h("button", { type: "button", class: "btn-secondary btn-small", "data-action": "open-teacher" }, t("viewTeacher")),
    )));
}

// One teacher in full: profile, prices, upcoming classes, and the switch.
function renderTeacher() {
  const tr = state.selected;
  const section = (title, ...children) => h("div", { class: "policy" }, h("h3", {}, title), ...children);
  const row = (label, value) => h("p", {}, h("strong", {}, `${label}: `), value || "—");
  // Group prices are the same for every party size (the teacher page saves
  // them that way), so each length is shown once, from party size 1.
  const shown = tr.price_list
    .filter((p) => p.kind === "private" || p.party_size === 1)
    .map((p) => `${p.kind === "group" ? t("priceRowGroup") : t("priceRowPrivate", p.party_size)} · ` +
                `${p.duration_minutes} min · ${money(p.price_per_person_cents)}`);

  const active = tr.status === "active";
  const button = h("button", {
    type: "button", class: active ? "btn-danger" : "btn-primary",
    "data-action": active ? "deactivate" : "activate", disabled: state.busy ? "" : null,
  }, t(active ? "deactivateTeacher" : "activateTeacher"));

  const refusal = state.refusal
    ? h("div", { class: "status is-error" },
        h("p", {}, t("deactivateRefused")),
        h("ul", {}, ...state.refusal.classes.map((c) => h("li", {}, `${classTitle(c)} · ${whenText(c)} · ${t("seatsBooked", c.seats_taken)}`))))
    : "";

  document.querySelector("#teacher-view").replaceChildren(
    h("button", { type: "button", class: "link-button", "data-action": "back-to-list" }, t("allTeachers")),
    h("h2", {}, tr.display_name, " ", statusTag(tr.status)),
    h("div", { class: "admin-actions" }, button, h("span", { class: "muted small" }, t(active ? "deactivateHint" : "activateHint"))),
    refusal,
    section(t("profileTitle"),
      row(t("emailLabel"), tr.email),
      row(t("timezoneLabel"), tr.timezone),
      row(t("profileBreak"), `${tr.break_minutes} min`),
      row(t("adminLengths"), tr.offered_lengths.map((l) => `${l} min`).join(", ")),
      row(t("adminBioEN"), tr.bio_en),
      row(t("adminBioES"), tr.bio_es)),
    section(t("adminPrices"), shown.length ? h("ul", {}, ...shown.map((line) => h("li", {}, line))) : h("p", { class: "muted" }, t("noPrices"))),
    section(t("upcomingClassesTitle"), tr.classes.length
      ? h("ul", {}, ...tr.classes.map((c) => h("li", {}, `${classTitle(c)} · ${whenText(c)} · ${t("seatsBooked", c.seats_taken)}`)))
      : h("p", { class: "muted" }, t("noClasses"))),
  );
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
  if (action === "open-teacher") openTeacher(e.target.closest("[data-teacher-id]").dataset.teacherId);
  if (action === "back-to-list") { state.selected = null; state.refusal = null; render(); }
  if (action === "toggle-inactive") { state.showInactive = !state.showInactive; render(); }
  if (action === "activate" && !state.busy) setStatus(true);
  if (action === "deactivate" && !state.busy) setStatus(false);
});

render();
load();
