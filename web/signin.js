// Sign-in page, opened from the emailed link: /signin.html#token=...
//
// Why a button instead of signing in as soon as the page opens: email security
// scanners open links automatically. If opening the link were enough, the
// scanner would use up the one-time token before the person ever clicked.
// Scanners don't click buttons, so the token is only spent by a real person.

const state = {
  uiLang: initialUiLang(),
  token: tokenFromLink(),
  status: "ready",           // "ready" | "verifying" | "missing" | "invalid" | "signedIn"
                             // | "relinking" | "relinked" | "error"
  maskedEmail: "",           // where a fresh link went ("m•••@yahoo.com")
  error: null,               // { key, requestId? } when status is "error"
};
if (!state.token) state.status = "missing";

const bodyEl = document.querySelector("#signin-body");

// Read the token from the part after "#" (which browsers never send to any
// server), then remove it from the address bar and history. It stays only in
// this page's memory, so it isn't left in history or seen on a shared screen.
function tokenFromLink() {
  const token = new URLSearchParams(location.hash.slice(1)).get("token");
  if (location.hash) history.replaceState(null, "", location.pathname);
  return token;
}

async function verify() {
  state.status = "verifying";
  render();

  // The browser's time zone goes along, so emails can show class times in it
  // (the server keeps it only if the account doesn't have one yet).
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const res = await api("POST", "/auth/verify", { token: state.token, timezone });
  if (res.ok) {
    // The server set the session cookie. replace() (not assign) so the back
    // button doesn't return to this page with its spent token.
    location.replace("/");
    return;
  }
  if (res.status === 400) {
    // A dead link. Already signed in on this browser? Then nothing's wrong.
    const me = await api("GET", "/me");
    state.status = me.ok ? "signedIn" : "invalid";
  } else {
    state.status = "error";
    state.error = res.status === 403
      ? { key: "accountDisabled" }
      : { key: "somethingWrong", requestId: res.requestId };
  }
  render();
}

// "Send me a new link": the old link knows its address, so nobody types it again.
async function relink() {
  state.status = "relinking";
  render();
  const res = await api("POST", "/auth/relink", { token: state.token });
  if (res.ok) {
    state.status = "relinked";
    state.maskedEmail = res.data.masked_email;
  } else if (res.data.code === "relink_unknown") {
    state.status = "missing";   // too old to know its address: the email box on the main page
  } else {
    state.status = "error";
    state.error = res.data.code === "too_many_links"
      ? { key: "tooManyLinks" }
      : { key: "somethingWrong", requestId: res.requestId };
  }
  render();
}

function render() {
  renderStaticText();
  bodyEl.replaceChildren();
  const newLink = () => h("p", {}, h("a", { href: "/#signin" }, t("requestNewLink")));

  switch (state.status) {
    case "ready":
    case "verifying":
      bodyEl.append(
        h("p", {}, t("signinPageIntro")),
        h("button", {
          type: "button", class: "btn-primary", "data-action": "verify",
          disabled: state.status === "verifying" ? "" : null,
        }, state.status === "verifying" ? t("signingIn") : t("signIn")),
      );
      break;
    case "missing":
      bodyEl.append(h("p", {}, t("linkMissing")), newLink());
      break;
    case "invalid":
    case "relinking":
      bodyEl.append(
        h("p", { class: "form-error" }, t("linkInvalid")),
        h("button", { type: "button", class: "btn-primary", "data-action": "relink",
          disabled: state.status === "relinking" ? "" : null }, state.status === "relinking" ? t("sending") : t("sendNewLink")),
      );
      break;
    case "relinked":
      bodyEl.append(h("p", {}, t("newLinkSent", state.maskedEmail)), h("p", { class: "muted small" }, t("newLinkHint")));
      break;
    case "signedIn":
      bodyEl.append(h("p", {}, t("alreadySignedIn")),
        h("a", { class: "btn-primary", href: "/" }, t("goToSite")));
      break;
    case "error": {
      const ref = state.error.requestId ? ` ${t("reference")}: ${state.error.requestId}` : "";
      bodyEl.append(h("p", { class: "form-error" }, t(state.error.key) + ref));
      // A server hiccup didn't use up the token (the transaction rolled back), so retrying is fine.
      if (state.error.key === "somethingWrong") {
        bodyEl.append(h("button", { type: "button", class: "btn-primary", "data-action": "verify" }, t("retry")));
      }
      break;
    }
  }
}

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) {
    render();
    return;
  }
  const action = e.target.closest("[data-action]")?.dataset.action;
  if (action === "verify") verify();
  if (action === "relink" && state.status !== "relinking") relink();
});

// A newer link opened in this same tab (mail apps on phones often reuse an
// open tab): only the part after "#" differs, so the browser doesn't reload
// the page, it just fires "hashchange". Pick up the new token and start over.
window.addEventListener("hashchange", () => {
  const token = tokenFromLink();
  if (!token) return;
  state.token = token;
  state.status = "ready";
  state.error = null;
  render();
});

render();
