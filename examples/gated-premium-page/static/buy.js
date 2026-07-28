// NMI tokenized-vault checkout, per docs/frontend-integration.md:
// delegated token from our backend -> Collect.js tokenizes the card ->
// POST /v1/me/checkout with the one-time payment_token -> /premium.
let cfg, priceID, cached;

// Fetch-and-cache delegated-token helper (docs/frontend-integration.md).
async function delegatedToken() {
  if (cached && cached.exp - 30_000 > Date.now()) return cached.token;
  const res = await fetch("/api/token");
  if (!res.ok) { location = "/"; throw new Error("not logged in"); }
  const { token, expires_in } = await res.json();
  cached = { token, exp: Date.now() + expires_in * 1000 };
  return token;
}

function setStatus(msg) { document.getElementById("status").textContent = msg; }

async function main() {
  cfg = await (await fetch("/api/config")).json();

  // Public catalog, no auth: GET /v1/products (active prices embedded).
  const res = await fetch(cfg.openrails_base_url + "/v1/products");
  const products = (await res.json()).data ?? [];
  const box = document.getElementById("products");
  box.textContent = "";
  for (const p of products) {
    for (const price of p.prices ?? []) {
      if (price.providers?.length && !price.providers.includes(cfg.psp)) continue;
      const b = document.createElement("button");
      b.className = "price";
      b.textContent = `${p.name} — $${(price.unit_amount / 1_000_000).toFixed(2)} ` +
        (price.recurring ? "(recurring)" : "(one-time)");
      b.onclick = () => {
        priceID = price.id;
        document.querySelectorAll(".price").forEach(x => x.classList.remove("selected"));
        b.classList.add("selected");
        document.getElementById("pay").disabled = false;
      };
      box.appendChild(b);
    }
  }
  if (!box.childElementCount) box.textContent = "No purchasable prices for PSP " + cfg.psp;

  // Collect.js needs its public tokenization key as a data- attribute.
  const s = document.createElement("script");
  s.src = cfg.tokenization_url;
  s.dataset.tokenizationKey = cfg.tokenization_key;
  s.onload = () => CollectJS.configure({
    variant: "inline",
    fields: {
      ccnumber: { selector: "#ccnumber", placeholder: "Card number" },
      ccexp: { selector: "#ccexp", placeholder: "MM / YY" },
      cvv: { selector: "#cvv", placeholder: "CVV" },
    },
    callback: (resp) => checkout(resp.token, resp.card),
  });
  document.head.appendChild(s);

  document.getElementById("pay").onclick = () => {
    setStatus("Tokenizing card…");
    CollectJS.startPaymentRequest();
  };
}

async function checkout(paymentToken, card) {
  setStatus("Charging…");
  // Optional display metadata from Collect.js (masked number/type/exp) so the
  // saved payment method lists with a brand + last four.
  const meta = {};
  if (card?.number) meta.last_four = card.number.slice(-4);
  if (card?.type) meta.card_type = card.type;
  if (card?.exp?.length === 4) meta.expiry_date = card.exp.slice(0, 2) + "/" + card.exp.slice(2);
  const res = await fetch(cfg.openrails_base_url + "/v1/me/checkout", {
    method: "POST",
    headers: {
      "Authorization": "Bearer " + await delegatedToken(),
      "Content-Type": "application/json",
      "Idempotency-Key": crypto.randomUUID(), // retries replay, never double-charge
    },
    body: JSON.stringify({
      price_id: priceID,
      // cfg.rail (not cfg.psp): the engine's checkout gate currently rejects
      // PSP-key wire values — see the NMI_RAIL note in main.go.
      payment: { rail: cfg.rail, payment_token: paymentToken, ...meta },
    }),
  });
  const session = await res.json();
  if (!res.ok) {
    setStatus("Checkout failed: " + (session.error?.message ?? res.status));
    return;
  }
  // NMI tokenized-vault checkout is synchronous; polling is the generic
  // fallback for webhook-finalized flows (redirect rails).
  if (session.status === "succeeded") { location = "/premium"; return; }
  setStatus("Waiting for payment to finalize…");
  for (let i = 0; i < 30; i++) {
    await new Promise(r => setTimeout(r, 2000));
    const poll = await fetch(cfg.openrails_base_url + "/v1/me/checkout/" + session.id, {
      headers: { "Authorization": "Bearer " + await delegatedToken() },
    });
    const state = await poll.json();
    if (state.status === "succeeded") { location = "/premium"; return; }
    if (state.status === "failed" || state.status === "expired") {
      setStatus("Payment " + state.status);
      return;
    }
  }
  setStatus("Timed out waiting for payment — check /premium in a moment.");
}

main().catch(e => setStatus(e.message));
