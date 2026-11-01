// Our Teachers page: who teaches here, in the visitor's language. Public.
// Same pattern as the other pages: state → render() → one click listener.

const state = {
  uiLang: initialUiLang(),
  status: "loading",   // "loading" | "error" | "ready"
  error: null,         // { requestId } when status is "error"
  teachers: [],        // GET /teachers
};

async function load() {
  const res = await api("GET", "/teachers");
  if (res.ok) {
    state.teachers = res.data;
    state.status = "ready";
  } else {
    state.status = "error";
    state.error = { requestId: res.requestId };
  }
  render();
}

function render() {
  renderStaticText();
  const statusEl = document.querySelector("#status");
  const none = state.status === "ready" && state.teachers.length === 0;
  statusEl.hidden = state.status === "ready" && !none;
  statusEl.className = state.status === "error" ? "status is-error" : "status";
  statusEl.textContent =
    state.status === "loading" ? t("loading") :
    state.status === "error" ? `${t("loadError")} ${t("reference")}: ${state.error.requestId}` :
    none ? t("teachersNone") : "";
  document.querySelector("#teachers").replaceChildren(...state.teachers.map((tr) => {
    // The bio in the page's language, or the other one if there's only that.
    const bio = state.uiLang === "es" ? tr.bio_es || tr.bio_en : tr.bio_en || tr.bio_es;
    return h("li", { class: "teacher-card" },
      tr.photo_id ? photoImg(tr.photo_id, tr.name, "photo") : h("div", { class: "photo photo-empty", "aria-hidden": "true" }),
      h("div", { class: "teacher-about" },
        h("h2", {}, tr.name),
        bio ? h("p", { class: "teacher-bio" }, bio) : ""));
  }));
}

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) render();
});

render();
load();
