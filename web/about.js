// About page: plain text from the server, in the visitor's language. Admins can
// edit it here; every save is a new version on the server.
// Same pattern as the other pages: state → render() → one click listener.

const state = {
  uiLang: initialUiLang(),
  status: "loading",   // "loading" | "error" | "ready"
  error: null,         // { requestId } when status is "error"
  page: null,          // GET /about-page: { body_en, body_es, saved_at }
  isAdmin: false,      // from GET /me; only decides whether Edit shows
  editing: false,
  saving: false,
  saveError: null,     // { key, requestId? } when the last save failed
};

const formEl = document.querySelector("#about-form");

async function load() {
  const [page, me] = await Promise.all([api("GET", "/about-page"), api("GET", "/me")]);
  if (!page.ok) {
    state.status = "error";
    state.error = { requestId: page.requestId };
    return render();
  }
  state.page = page.data;
  state.isAdmin = me.ok && me.data.account.is_admin;
  state.status = "ready";
  render();
}

// Text → elements: a line starting with "# " is a heading, a blank line ends a
// paragraph, and other line breaks stay as typed. Everything goes in as text,
// so "<script>" typed here shows as those characters and can never run.
function aboutElements(text) {
  const out = [];
  let lines = [];
  const endParagraph = () => {
    if (lines.length) out.push(h("p", {}, lines.join("\n")));
    lines = [];
  };
  for (const line of text.split(/\r?\n/)) {
    if (line.startsWith("# ")) {
      endParagraph();
      out.push(h("h2", {}, line.slice(2).trim()));
    } else if (line.trim() === "") {
      endParagraph();
    } else {
      lines.push(line);
    }
  }
  endParagraph();
  return out;
}

function startEditing() {
  formEl.elements.body_en.value = state.page.body_en;
  formEl.elements.body_es.value = state.page.body_es;
  state.editing = true;
  state.saveError = null;
  render();
  formEl.elements.body_en.focus();
}

async function save() {
  const form = new FormData(formEl);
  state.saving = true;
  state.saveError = null;
  render();
  const res = await api("PUT", "/about-page", { body_en: form.get("body_en"), body_es: form.get("body_es") });
  state.saving = false;
  if (res.ok) {
    state.page = res.data;
    state.editing = false;
    showToast(t("aboutSaved"));
    window.scrollTo(0, 0);
  } else {
    state.saveError = res.data.code === "invalid_about" ? { key: "aboutErrText" }
      : res.status === 401 || res.status === 403 ? { key: "aboutErrNotAdmin" }
      : { key: "somethingWrong", requestId: res.requestId };
  }
  render();
}

function render() {
  renderStaticText();
  const statusEl = document.querySelector("#status");
  statusEl.hidden = state.status === "ready";
  statusEl.className = state.status === "error" ? "status is-error" : "status";
  statusEl.textContent =
    state.status === "loading" ? t("loading") :
    state.status === "error" ? `${t("loadError")} ${t("reference")}: ${state.error.requestId}` : "";
  const text = state.page ? (state.uiLang === "es" ? state.page.body_es : state.page.body_en) : "";
  document.querySelector("#about-text").replaceChildren(...aboutElements(text));
  document.querySelector("#about-edit").hidden = !state.isAdmin || state.editing || state.status !== "ready";
  formEl.hidden = !state.editing;
  const saveEl = document.querySelector("#about-save");
  saveEl.textContent = state.saving ? t("photoSaving") : t("aboutSave");
  saveEl.disabled = state.saving;
  const err = state.saveError;
  document.querySelector("#about-error").textContent =
    err ? t(err.key) + (err.requestId ? ` ${t("reference")}: ${err.requestId}` : "") : "";
}

let toastTimer;
function showToast(message) {
  const el = document.querySelector("#toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 4000);
}

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) return render();
  const action = e.target.closest("[data-action]")?.dataset.action;
  if (action === "edit") startEditing();
  if (action === "cancel-edit") { state.editing = false; state.saveError = null; render(); }
});

document.addEventListener("submit", (e) => {
  if (e.target !== formEl) return;
  e.preventDefault();
  if (!state.saving) save();
});

render();
load();
