// A friend's invitation to a private class: /join.html#token=...
//
// The token is read from after "#" (never sent to a server) and removed from
// the address bar. A friend who isn't signed in goes to the main page's
// sign-in; the login email usually opens in a new tab, so the token waits in
// localStorage (shared by the site's tabs), and the main page sends them back
// here once signed in (app.js). It's dropped after joining, when the link
// turns out to be no good, or after a day.

const pendingJoinKey = "pendingJoin";

const state = {
  uiLang: initialUiLang(),
  token: takeToken(),
  status: "loading",   // "loading" | "missing" | "invalid" | "ready" | "joining" | "error"
  preview: null,       // POST /seats/preview
  me: undefined,       // null = signed out, else the account with learners
  learnerId: null,     // who's coming
  error: null,         // { key, requestId? }
};

function takeToken() {
  const fromLink = new URLSearchParams(location.hash.slice(1)).get("token");
  if (location.hash) history.replaceState(null, "", location.pathname);
  if (fromLink) return fromLink;
  try {
    const saved = JSON.parse(localStorage.getItem(pendingJoinKey) || "null");
    if (saved && Date.now() - saved.savedAt < 86400000) return saved.token;
  } catch { /* storage blocked or garbled: no saved invitation */ }
  return null;
}

function remember(token) {
  try { localStorage.setItem(pendingJoinKey, JSON.stringify({ token, savedAt: Date.now() })); } catch { /* fine */ }
}
function forget() {
  try { localStorage.removeItem(pendingJoinKey); } catch { /* fine */ }
}

async function load() {
  if (!state.token) { state.status = "missing"; return render(); }
  const [preview, me] = await Promise.all([api("POST", "/seats/preview", { token: state.token }), api("GET", "/me")]);
  if (!preview.ok) {
    forget();
    state.status = preview.status === 404 ? "invalid" : "error";
    state.error = { key: "somethingWrong", requestId: preview.requestId };
    return render();
  }
  state.preview = preview.data;
  state.me = me.ok ? { ...me.data.account, learners: me.data.learners } : null;
  if (state.me?.learners.length === 1) state.learnerId = state.me.learners[0].id;   // only one person: preselect
  state.status = "ready";
  render();
}

const joinErrorKeys = {
  seat_taken: "joinSeatTaken",
  seat_link_invalid: "joinLinkInvalid",
  already_in_class: "joinAlreadyIn",
  not_changeable: "joinClassGone",
  unknown_learner: "somethingWrong",
};

async function join() {
  state.status = "joining";
  state.error = null;
  render();
  const res = await api("POST", "/seats/join", { token: state.token, learner_id: state.learnerId });
  if (res.ok) {
    forget();
    location.replace("/#joined");   // the main page says "You're in!", and My bookings shows the class
    return;
  }
  if (res.status === 401) { state.me = null; state.status = "ready"; return render(); }
  const key = joinErrorKeys[res.data.code];
  if (key && key !== "somethingWrong") forget();   // this link won't work again
  state.status = "error";
  state.error = key ? { key, requestId: key === "somethingWrong" ? res.requestId : null } : { key: "somethingWrong", requestId: res.requestId };
  render();
}

function render() {
  renderStaticText();
  const body = document.querySelector("#join-body");
  body.replaceChildren();
  const p = state.preview;
  if (p) {
    const start = new Date(p.starts_at);
    const date = new Intl.DateTimeFormat(state.uiLang, { weekday: "long", month: "long", day: "numeric" }).format(start);
    const time = new Intl.DateTimeFormat(state.uiLang, { hour: "numeric", minute: "2-digit" }).format(start);
    body.append(
      h("p", {}, t("joinInvitedBy", p.invited_by)),
      h("p", { class: "book-price" },
        `${p.title ?? t("privateClass")} · ${p.target_language === "es" ? t("learnSpanish") : t("learnEnglish")}`),
      h("p", {}, `${date} · ${time} · ${t("minutes", p.duration_minutes)}`),
      h("p", { class: "teacher-name" }, t("withTeacher", p.teacher_name)),
      h("p", { class: "muted small" }, t("joinFree")),
    );
  }
  switch (state.status) {
    case "loading":
      body.append(h("p", {}, t("loading")));
      break;
    case "missing":
      body.append(h("p", {}, t("joinMissing")));
      break;
    case "invalid":
      body.append(h("p", { class: "form-error" }, t("joinLinkInvalid")));
      break;
    case "ready":
    case "joining":
    case "error":
      if (state.error) {
        const ref = state.error.requestId ? ` ${t("reference")}: ${state.error.requestId}` : "";
        body.append(h("p", { class: "form-error" }, t(state.error.key) + ref));
      }
      if (!p) break;
      if (state.me === null) {
        body.append(h("p", {}, t("joinSignInFirst")),
          h("button", { type: "button", class: "btn-primary", "data-action": "sign-in" }, t("signIn")));
        break;
      }
      body.append(h("fieldset", { class: "attendees" },
        h("legend", {}, t("joinWho")),
        ...state.me.learners.map((l) => h("label", { class: "attendee" },
          h("input", { type: "radio", name: "learner", value: l.id, checked: l.id === state.learnerId ? "" : null }),
          l.is_minor ? `${l.display_name} (${t("child")})` : l.display_name))));
      body.append(h("button", {
        type: "button", class: "btn-primary", "data-action": "join",
        disabled: !state.learnerId || state.status === "joining" ? "" : null,
      }, state.status === "joining" ? t("joining") : t("joinButton")));
  }
}

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) return render();
  const action = e.target.closest("[data-action]")?.dataset.action;
  if (action === "sign-in") {
    remember(state.token);              // waits here while the login email goes back and forth
    location.assign("/#signin");        // the main page opens its sign-in dialog
  }
  if (action === "join" && state.status !== "joining") join();
});

document.addEventListener("change", (e) => {
  if (e.target.name === "learner") { state.learnerId = e.target.value; render(); }
});

render();
load();
