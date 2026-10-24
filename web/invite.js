// Teacher invitation page, opened from the emailed (or shared) link:
// /invite.html#token=...  Same reasons as the sign-in page for a button rather
// than accepting on load: email scanners open links, but don't click buttons.
// Accepting signs the person in, so on success we go straight to the teacher page.

const state = {
  uiLang: initialUiLang(),
  token: tokenFromLink(),
  status: "ready",           // "ready" | "sending" | "missing" | "error"
  error: null,               // { key, requestId? } when status is "error"
};
if (!state.token) state.status = "missing";

const bodyEl = document.querySelector("#invite-body");

// The token is after "#" (never sent to a server); read it, then remove it
// from the address bar and history.
function tokenFromLink() {
  const token = new URLSearchParams(location.hash.slice(1)).get("token");
  if (location.hash) history.replaceState(null, "", location.pathname);
  return token;
}

const inviteErrorKeys = {
  invite_used: "inviteUsed",
  invite_expired: "inviteExpired",
  invite_invalid: "inviteInvalid",
  account_disabled: "accountDisabled",
};

async function acceptInvite() {
  state.status = "sending";
  render();
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const res = await api("POST", "/invites/accept", { token: state.token, timezone });
  if (res.ok) {
    location.replace("teacher.html");   // replace: Back doesn't return to a spent link
    return;
  }
  const key = inviteErrorKeys[res.data.code];
  state.status = "error";
  state.error = key ? { key } : { key: "somethingWrong", requestId: res.requestId };
  render();
}

function render() {
  renderStaticText();
  bodyEl.replaceChildren();
  switch (state.status) {
    case "ready":
    case "sending":
      bodyEl.append(
        h("p", {}, t("inviteIntro")),
        h("button", {
          type: "button", class: "btn-primary", "data-action": "accept",
          disabled: state.status === "sending" ? "" : null,
        }, state.status === "sending" ? t("joining") : t("joinAsTeacher")),
      );
      break;
    case "missing":
      bodyEl.append(h("p", {}, t("inviteMissing")));
      break;
    case "error": {
      const ref = state.error.requestId ? ` ${t("reference")}: ${state.error.requestId}` : "";
      bodyEl.append(h("p", { class: "form-error" }, t(state.error.key) + ref));
      // Already used: most likely by this person a moment ago (a double click).
      if (state.error.key === "inviteUsed") bodyEl.append(h("p", {}, h("a", { class: "link", href: "teacher.html" }, t("teacherLink"))));
      if (state.error.key === "somethingWrong") {
        bodyEl.append(h("button", { type: "button", class: "btn-primary", "data-action": "accept" }, t("retry")));
      }
      break;
    }
  }
}

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) return render();
  if (e.target.closest("[data-action]")?.dataset.action === "accept" && state.status !== "sending") acceptInvite();
});

render();
