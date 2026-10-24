// Policies page: the rules, in the visitor's language, with every number taken
// from the server (GET /policies reads the same constants and settings the
// code enforces; GET /credit-packs the packs on sale). Nothing here is a
// number of its own, so the page can't promise what the code doesn't do.
// Same pattern as the other pages: state → render() → one click listener.

const state = {
  uiLang: initialUiLang(),
  status: "loading",   // "loading" | "error" | "ready"
  error: null,         // { requestId } when status is "error"
  policies: null,      // GET /policies
  packs: [],           // GET /credit-packs
};

const contactEmail = "hola@hablemos.live";

async function load() {
  const [policies, packs] = await Promise.all([api("GET", "/policies"), api("GET", "/credit-packs")]);
  if (!policies.ok || !packs.ok) {
    state.status = "error";
    state.error = { requestId: (policies.ok ? packs : policies).requestId };
  } else {
    state.policies = policies.data;
    state.packs = packs.data;
    state.status = "ready";
  }
  render();
}

// money(5500) → "$55.00" (English) or "55,00 US$" (Spanish), as on the main page.
function money(cents, currency = "USD") {
  return new Intl.NumberFormat(state.uiLang, { style: "currency", currency }).format(cents / 100);
}

function render() {
  renderStaticText();
  const statusEl = document.querySelector("#status");
  statusEl.className = state.status === "error" ? "status is-error" : "status";
  statusEl.textContent =
    state.status === "loading" ? t("loading") :
    state.status === "error" ? `${t("loadError")} ${t("reference")}: ${state.error.requestId}` : "";
  document.querySelector("#policies").replaceChildren(...(state.status === "ready" ? sections() : []));
}

// One <section> per topic: a heading and its points (each a translated
// sentence filled in with the server's numbers).
function sections() {
  const p = state.policies;
  const packs = state.packs.map((k) => t("polPack", money(k.price_cents, k.currency), money(k.credit_cents, k.currency)));
  const topics = [
    ["polCancelTitle", [
      t("polCancelEarly", p.full_refund_hours),
      t("polCancelLate", p.full_refund_hours, p.late_refund_percent),
      t("polCancelStarted"),
      t("polCancelAsCredit"),
    ]],
    ["polGroupTitle", [
      t("polGroupMinimum", p.group_min_students, p.group_decision_hours),
      t("polGroupConfirmed"),
    ]],
    ["polPrivateTitle", [
      t("polPrivateNotice", p.private_notice_minutes),
      t("polPrivateParty", p.max_party_size),
    ]],
    ["polBreakTitle", [t("polBreak", p.break_minutes)]],
    ["polCreditTitle", [
      t("polCreditPacks", packs.join(" · ")),
      t("polCreditExpiry", p.credit_expire_months),
      t("polCreditReminder", p.credit_warn_days_before),
      t("polCreditCash"),
    ]],
    ["polKidsTitle", [t("polKidsProfiles"), t("polKidsAudience"), t("polKidsParent")]],
  ];
  const list = topics.map(([title, points]) =>
    h("section", { class: "policy" },
      h("h2", {}, t(title)),
      h("ul", {}, ...points.map((text) => h("li", {}, text))),
    ));
  // Contact: a mailto: link (opens the visitor's email program, addressed to us).
  list.push(h("section", { class: "policy" },
    h("h2", {}, t("polContactTitle")),
    h("p", {}, t("polContact"), " ", h("a", { class: "link", href: `mailto:${contactEmail}` }, contactEmail)),
  ));
  return list;
}

document.addEventListener("click", (e) => {
  if (handleLangClick(e)) render();   // text only: no refetch
});

render();
load();
