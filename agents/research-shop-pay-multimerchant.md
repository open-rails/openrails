# Shop Pay as a Multi-Merchant Wallet: How It Works & What It Would Take to Build One

*Research report for OpenRails. Date: 2026-06-14.*
*Scope: (a) how Shop Pay / Shopify Payments achieve cross-merchant stored-card checkout; (b) the model taxonomy and the specific licenses + bank relationships each requires; (c) the buy-vs-partner provider landscape; (d) a ranked recommendation for our NMI + Stripe Connect + Solana stack.*

> **Lawyer flag up front.** Everything below about licensing (PayFac registration, MTLs, agent-of-payee exemptions, MoR liability) is research-grade, not legal advice. The line between "exempt payment processor" and "money transmitter" is state-specific and fact-specific, and the card-network rules around aggregation are contractual and enforced unevenly. Before committing to any path that holds or routes funds across merchants, get a payments/fintech attorney to opine on the exact funds flow.

---

## 1. How Shop Pay actually does cross-merchant stored-card checkout

### 1.1 The key distinction: Shop Pay ≠ Shopify Payments

These are two different things and conflating them is the most common error:

- **Shopify Payments** is the *acquiring / processing* layer. It is a **payment facilitator (PayFac)**: Shopify holds a master merchant account and onboards stores as **sub-merchants** under it. It is widely described as originally a white-labeled Stripe integration, and Shopify has since added Adyen as an acquiring/processing partner at scale. ([merchantcostconsulting.com](https://merchantcostconsulting.com/lower-credit-card-processing-fees/shop-pay-vs-shopify-payments/))
- **Shop Pay** is the *accelerated-checkout + stored-credentials* layer (the consumer "wallet"). It is **not itself a payment processor**; it sits on top of whatever gateway/acquirer is configured and supplies the saved card + identity. ([merchantcostconsulting.com](https://merchantcostconsulting.com/lower-credit-card-processing-fees/shop-pay-vs-shopify-payments/), [pagefly.io](https://pagefly.io/blogs/shopify/what-is-shop-pay))

So the "multi-merchant" magic is really **Shop Pay (the consumer credential vault + identity) feeding into Shopify Payments (the PayFac acquiring rail) and other gateways.**

### 1.2 The customer side: one account, SMS-OTP auth, vaulted credentials

- Using Shop Pay creates a **consumer account keyed to email + phone number**, independent of any individual store's account settings. That account is what lets saved details be reused across different merchants. ([community.shopify.com](https://community.shopify.com/t/can-i-enable-shop-pay-and-payments-without-the-remember-me-option-at-checkout/65752), [pagefly.io](https://pagefly.io/blogs/shopify/what-is-shop-pay))
- **Authentication is SMS OTP**: a 6-digit code texted to the phone on file authorizes the checkout/payment. There is no merchant-side password; the phone is the auth factor. ([help.shop.app](https://help.shop.app/en/shop/shop-pay-settings/security/verification-code), [adaptivpayments.com](https://adaptivpayments.com/blog/is-shop-pay-safe))
- Card data is **vaulted centrally by Shopify, not on merchant servers** — "personal details not stored on merchant servers," PCI-compliant encryption + SMS verification. ([adaptivpayments.com](https://adaptivpayments.com/blog/is-shop-pay-safe))

### 1.3 The token side: network tokenization + PAR

- Card networks (Visa, Mastercard, Amex) issue **network tokens** under EMVCo specs that replace the PAN; a token can be scoped to a domain (merchant or device). Tokens "stay live," so the same customer can be charged repeatedly without re-entering the card. ([shopify.com/blog/payment-tokenization](https://www.shopify.com/blog/payment-tokenization))
- Crucially, **network tokens are merchant-scoped**: one PAN yields *different* tokens for different merchants. ([shopify.com/blog/payment-tokenization](https://www.shopify.com/blog/payment-tokenization), [paypal.com](https://www.paypal.com/us/brc/article/network-tokenization))
- The glue that lets a wallet recognize "this is the same human/card across merchants" without seeing the PAN is the **Payment Account Reference (PAR)** — a single ID that links all of a card's tokens back to one underlying account. ([shopify.com/blog/payment-tokenization](https://www.shopify.com/blog/payment-tokenization))

### 1.4 So what *is* Shopify in the funds flow?

Putting it together, the honest answer is **"both, depending on layer"**:

1. **Acquiring/funds movement: Shopify Payments is a PayFac (payment facilitator / aggregator).** Stores are sub-merchants under Shopify's master account; Shopify (via its sponsor bank + processor partners) underwrites them, settles funds to them, and is registered with the networks as a PayFac. The *seller of record is still the store* in the standard model — Shopify is generally **not** the merchant of record for a typical store's sales. ([dodopayments.com/blogs/shopify-merchant-of-record](https://dodopayments.com/blogs/shopify-merchant-of-record), [gappgroup.com](https://gappgroup.com/blog/merchant-of-record-shopify/))
2. **Stored-credential reuse: network tokenization + a central consumer identity (Shop Pay account).** Shopify holds the card once (PCI L1 vault), mints network tokens, and uses the PAR/identity layer so that when the customer lands on *any* participating store the saved instrument can be re-presented and charged **on behalf of that store** — not by Shopify-as-seller.

The cross-merchant reach is now explicit, not theoretical:
- Shop Pay was opened to **non-Shopify merchants selling on Facebook/Google in 2021** — the first Shopify product offered off-platform. ([techcrunch.com](https://techcrunch.com/2021/06/15/shopify-expands-its-one-click-checkout-shop-pay-to-any-merchant-on-facebook-or-google/))
- In 2023 it became a **drop-in JS "Commerce Component"** any retailer can embed on non-Shopify infrastructure. ([siliconangle.com](https://siliconangle.com/2023/06/21/shopify-brings-shop-pay-checkout-service-external-e-commerce-websites/), [growpay.co](https://www.growpay.co/blog/products/article/shop-pay-can-now-be-integrated-with-other-commerce-platforms/))

**Net mental model for us:** Shop Pay is *not* a single merchant pretending to be a wallet, and it's *not* a money transmitter holding everyone's balance. It is **(consumer identity + central PCI vault + network tokens)** layered on top of a **PayFac acquiring rail (Shopify Payments)**, with each charge executed as the destination store's transaction. That is the architecture we'd be cloning.

> ⚠️ **Uncertainty:** Shopify does not publish the exact internal token-handoff for cross-store reuse, and network-token rules technically scope tokens per merchant. The most defensible interpretation of public sources is that Shopify holds the credential centrally (it is the entity with PCI scope and the network-tokenization relationship) and provisions/charges per destination sub-merchant. Treat the precise token mechanics as inferred, not documented.

---

## 2. Model taxonomy: the four ways to do cross-merchant payment, and the licenses each needs

| Model | What you become | Funds flow | Core licenses / registrations | Bank relationship |
|---|---|---|---|---|
| **Payment Facilitator (PayFac)** | The master merchant; stores are *sub-merchants* under you | You can route/settle to sub-merchants; you underwrite them | Visa/MC **PayFac registration**, **PCI-DSS Level 1**, KYC/KYB/AML/BSA program, BRAM/risk program. **MTL often required if you hold/control funds** (depends on flow) | **Sponsor (acquiring) bank** mandatory — provides BIN + network access |
| **Merchant of Record (MoR)** | The *legal seller*; you buy from the underlying merchant and resell | You are the merchant on the card statement; you remit net to the seller | No PayFac registration needed *if genuinely the seller*; you own **sales tax/VAT, chargebacks, consumer-protection compliance**, PCI. **MTL risk if you're not really the seller** | Standard merchant account/acquirer (you're just a big merchant) |
| **Payment Aggregator** | Essentially the legacy name for PayFac; networks treat similarly | Same as PayFac | Same as PayFac | Sponsor bank |
| **Network-token / credential-on-file reuse** | A technology layer, *not* a funds-flow role | Each merchant charges independently; you never hold funds | **PCI-DSS L1** (if you vault PANs) + network-tokenization enrollment. *No MTL, no PayFac registration if you never touch funds* | None required for the token layer itself |

### 2.1 Payment Facilitator (PayFac) — the full-control, full-burden path

To register as a PayFac with Visa/Mastercard you need:
- A **sponsoring acquirer bank** that gives you the BIN and the acquiring relationship (named sponsor banks include Wells Fargo, Fifth Third, Pathward, Esquire, Chesapeake). ([infinicept.com](https://infinicept.com/payment-facilitator/learn/get-started/what-is-the-relationship-between-payment-facilitators-and-merchant-acquirers/), [payram.com](https://www.payram.com/blog/how-to-become-a-payment-facilitator))
- **Network registration**: Mastercard requires PayFac registration once a single sub-merchant exceeds **$100K/yr**, plus certification on their **BRAM** risk program; Visa has an analogous program with minimum net-worth requirements. ([usio.com](https://usio.com/how-to-become-a-payment-facilitator/), [datacapsystems.com](https://datacapsystems.com/blog/must-have-requirements-for-payfacs))
- **PCI-DSS Level 1** certification. ([usio.com](https://usio.com/how-to-become-a-payment-facilitator/), [pxp.io](https://www.pxp.io/payments-glossary/payment-facilitator-compliance))
- **KYC/KYB + AML/BSA + OFAC** screening and ongoing risk monitoring on every sub-merchant you onboard. ([usio.com](https://usio.com/how-to-become-a-payment-facilitator/))
- **Money Transmitter Licenses** become a live question the moment your funds flow has you *holding or controlling* sub-merchant funds before settlement (more in §2.3).

### 2.2 Merchant of Record (MoR) — you become the seller (Paddle / Lemon Squeezy / Stripe-MoR)

- The MoR **becomes the legal seller**: collects payment, remits sales tax/VAT across jurisdictions, absorbs chargeback liability, pays the underlying seller a net amount. Paddle/Lemon Squeezy charge ~5% + 50¢ for this. ([paddle.com](https://www.paddle.com/blog/payfac-vs-merchant-of-record), [dodopayments.com/blogs/best-merchant-of-record-platforms](https://dodopayments.com/blogs/best-merchant-of-record-platforms))
- **The regulatory catch — the FTC/Paddle action (2025).** The FTC settled with Paddle for **$5M** and argued Paddle was **not actually the seller** ("did not purport to be the actual seller … characterized itself as providing end-to-end payment processing") and therefore **should have registered as a payment facilitator/aggregator** rather than hiding behind MoR. The takeaway: **MoR only legitimately avoids PayFac registration if you are genuinely the merchant** — controlling pricing, refunds, fulfillment, customer service. If the underlying merchant controls all of that and you're just routing money, regulators and card networks will treat you as an unregistered aggregator. ([mofo.com](https://www.mofo.com/resources/insights/250707-the-ftc-gives-the-merchant-of-record-model-a-paddling))
- This matters enormously for us: a "wallet that charges on behalf of merchant X" looks like aggregation, **not** like genuine MoR, unless we truly resell.

### 2.3 Money Transmitter Licenses (MTL) — the holding-funds tripwire

- Triggered when you **receive money from a payer to transmit to a third party** and **hold/control** it. The classic escape hatches:
  - **Agent-of-the-payee exemption**: if you're contractually the *payee's* agent (the merchant authorizes you to receive funds on their behalf, and receipt by you = receipt by the merchant), many states do not require an MTL. The CSBS maintains a **state-by-state agent-of-payee exemption map** — it is *not* uniform. ([csbs.org](https://www.csbs.org/agent-payee-exemption-map), [dfpi.ca.gov](https://dfpi.ca.gov/wp-content/uploads/sites/337/2019/05/PRO-07-17-Electronic-Transactions-Association.pdf))
  - **Payment-processor exemption**: processors that move money *directly* buyer→merchant without holding it (classic card processors/POS) are often exempt — but state laws vary. ([moderntreasury.com](https://www.moderntreasury.com/journal/how-do-money-transmission-laws-work))
  - **FBO ("for benefit of") account** structures are how fintechs hold pooled funds without (always) being the licensee — usually requires a bank partner whose license covers it.
- **Cost/timeline if you do need MTLs nationwide**: roughly **$240K–$475K+ to get licensed in all states**, **3–18 months per state** (NY can be 12–24 months), surety bonds **$10K–$1M per state**, plus legal counsel **$10K–$50K+**. This is the single biggest reason fintechs *rent* licenses rather than acquire them. ([brico.ai](https://www.brico.ai/post/how-much-do-mtls-cost), [innreg.com](https://www.innreg.com/blog/money-transmitter-license-steps-and-requirements))
- States have been adopting the **Money Transmission Modernization Act (MTMA / model law)** for some harmonization, but Cooley notes "harmonization remains elusive" — you still analyze state by state. ([cooley.com](https://www.cooley.com/news/insight/2024/2024-08-20-us-states-adopt-model-money-transmission-act-but-harmonization-remains-elusive))

### 2.4 PCI-DSS L1 + network tokenization — the part you (mostly) can't avoid but can offload

- If *you* store PANs, you're **PCI-DSS Level 1** (>6M txns/yr threshold, but anyone vaulting at scale). ([usio.com](https://usio.com/how-to-become-a-payment-facilitator/))
- **Network tokenization** is the clean way to enable cross-merchant reuse *without* you holding raw PANs: enroll the card with the networks, store tokens + PAR. But remember network tokens are **merchant-scoped**, so a true cross-merchant wallet still needs a central entity (you, or your processor) that holds the credential and provisions per-merchant tokens. ([shopify.com/blog/payment-tokenization](https://www.shopify.com/blog/payment-tokenization), [paypal.com](https://www.paypal.com/us/brc/article/network-tokenization))

### 2.5 KYC/AML/BSA/OFAC + Nacha/ACH

- Any model where you onboard merchants and route funds pulls in **KYC/KYB, AML/BSA program, OFAC screening**. ([usio.com](https://usio.com/how-to-become-a-payment-facilitator/))
- If you pay merchants out via bank transfer, you're subject to **Nacha/ACH** rules (origination agreements, return handling) — typically via your bank partner.

---

## 3. Buy vs. partner — the provider landscape and what each offloads

The whole industry exists to let you avoid §2's burden. Ranked by how much regulatory weight they take off you:

| Provider / model | What you can do | What it offloads | What you still own |
|---|---|---|---|
| **Stripe Connect** (Standard / Express / Custom) | Onboard connected sellers, route money, do direct/destination charges. **Stripe is the registered PayFac**; you ride their license + sponsor bank | Network registration, sponsor bank, most PCI scope, KYC tooling, much MTL exposure | Onboarding UX, some risk decisions; less control/economics than full PayFac. ([fiska.com](https://fiska.com/blog-articles/alternatives-becoming-a-payfac-for-saas/)) |
| **Stripe cross-account payment-method cloning** | Reuse a customer's saved card across *multiple connected accounts* (clone PaymentMethod, charge on behalf of each connected account) | The hard part of cross-merchant reuse — Stripe handles vault + token | **Marked legacy / "no longer recommended; support might end without notice."** Current path: "reuse payment information across connected accounts" via direct charges + cloning. Cards + us_bank_account only. ([docs.stripe.com/connect/cloning-customers-across-accounts](https://docs.stripe.com/connect/cloning-customers-across-accounts), [docs.stripe.com/connect/direct-charges-multiple-accounts](https://docs.stripe.com/connect/direct-charges-multiple-accounts)) |
| **Adyen for Platforms** | Marketplace/platform acquiring at scale (this is who Shopify itself uses at the high end) | Acquiring, license, PCI, much risk | Enterprise-grade lift; heavier integration |
| **Finix / Payrix (PayFac-as-a-Service)** | Onboard sub-merchants, process, **earn the PayFac economics** without registering yourself | Card-network relationships, underwriting infra, risk tooling, KYC automation | You still run onboarding, KYC flows, risk policy. Launch in weeks vs. 3–9 months for full PayFac. ([techbullion.com](https://techbullion.com/the-6-best-payfac-as-a-service-providers-for-platforms/), [usio.com](https://usio.com/payfac-as-a-service-vs-full-payment-facilitation-a-guide/)) |
| **Unit / other BaaS** | Hold funds in FBO accounts, issue accounts/cards under a bank partner's charter | The bank charter + much MTL exposure | Compliance program participation; bank-partner constraints |
| **Full PayFac (build it yourself)** | Total control + best economics | Nothing | Everything in §2.1–§2.5: registration, sponsor bank, PCI L1, KYC/AML, likely MTLs |

Key reality checks from the sources:
- PayFac-as-a-Service "allows platforms to onboard merchants, process transactions, and earn revenue **without taking on the regulatory burden** … the provider handling underwriting, risk management, and card network relationships" — but **you still build onboarding/KYC/risk policy**. ([techbullion.com](https://techbullion.com/the-6-best-payfac-as-a-service-providers-for-platforms/))
- Full PayFac registration + the dance with networks/sponsor bank is the **3–9+ month, six-figure** path; PFaaS compresses it to weeks. ([usio.com](https://usio.com/payfac-as-a-service-vs-full-payment-facilitation-a-guide/))

---

## 4. Concrete recommendation for OpenRails (NMI + Stripe Connect + Solana)

Our problem stated precisely: **today a vault is bound to one merchant** (NMI vault per merchant account; Stripe Customer per connected account). We want **one customer wallet usable across many merchants** — i.e., re-present a saved instrument and charge it *on behalf of whichever merchant the customer is buying from*.

### Ranked paths (regulatory burden → capability)

**Rank 1 — RECOMMENDED to start: Network-token / credential-on-file reuse on top of Stripe Connect.**
- *Mechanism:* hold the customer + payment method **on the OpenRails platform Stripe account** (one central Customer), then **charge on behalf of each connected merchant** using Stripe's "reuse payment information across connected accounts" flow (the supported successor to the legacy clone-customer feature). Each charge executes as the **connected merchant's** transaction → each merchant stays the seller of record, money settles to them, **no funds held by us**. ([docs.stripe.com/connect/direct-charges-multiple-accounts](https://docs.stripe.com/connect/direct-charges-multiple-accounts))
- *Regulatory burden:* **lowest.** No MTL (we never hold/route balances — Stripe Connect does), no separate PayFac registration (we ride Stripe's), Stripe carries PCI L1 and network-tokenization. We own KYC of connected accounts (Connect handles most) + our own consumer auth (build an SMS-OTP layer — the Shop Pay pattern).
- *Caveat:* the **explicit cross-account clone API is marked legacy** ("support might end without notice"). Build on the *current* direct-charges-with-cloning pattern and confirm roadmap with Stripe; don't architect on the deprecated call. ([docs.stripe.com/connect/cloning-customers-across-accounts](https://docs.stripe.com/connect/cloning-customers-across-accounts))
- *Capability ceiling:* this gives a genuine Shop-Pay-like wallet for the Stripe-Connect side of our book. It does **not** automatically extend to merchants we run only on **NMI** — NMI vaults are merchant-scoped and not natively cross-merchant, so NMI merchants would need either migration onto the Connect wallet or a separate Customer Vault arrangement. Flag this as the main architectural seam.

**Rank 2 — If we want full control + economics: PayFac-as-a-Service (Finix or Payrix).**
- Lets us own the wallet + onboarding + sub-merchant settlement and earn PayFac economics, **without** network registration, sponsor bank, or (mostly) MTLs — the PFaaS provider holds those. Weeks-to-months, not the full six-figure registration slog. ([techbullion.com](https://techbullion.com/the-6-best-payfac-as-a-service-providers-for-platforms/), [usio.com](https://usio.com/payfac-as-a-service-vs-full-payment-facilitation-a-guide/))
- Burden: we run KYC/risk policy + onboarding; provider runs the rest. Good if Rank 1's "you're stapled to Stripe's product roadmap" is unacceptable.

**Rank 3 — Merchant of Record (we become the seller).** 
- Cleanest *conceptually* for a wallet (one customer relationship, we charge, we remit to merchants), and avoids per-merchant card setup. **But the FTC/Paddle action is a direct warning:** if we don't genuinely become the seller (control pricing/refunds/fulfillment/support), regulators treat us as an unregistered aggregator and we inherit PayFac *and* potentially MTL obligations anyway — plus full sales-tax, chargeback, and consumer-protection liability across jurisdictions. Only viable if our business genuinely resells. High legal exposure. ([mofo.com](https://www.mofo.com/resources/insights/250707-the-ftc-gives-the-merchant-of-record-model-a-paddling), [paddle.com](https://www.paddle.com/blog/payfac-vs-merchant-of-record))

**Rank 4 — Full PayFac (register ourselves + sponsor bank + MTLs).** 
- Maximum control/economics, **maximum burden**: Visa/MC registration, sponsor bank, PCI L1, full KYC/AML/BSA program, and likely a multi-state MTL build at **$240K–$475K+ and 3–18 months/state** if our funds flow has us holding money. Only justifiable at very large scale. ([brico.ai](https://www.brico.ai/post/how-much-do-mtls-cost), [usio.com](https://usio.com/how-to-become-a-payment-facilitator/))

### Where Solana sidesteps all of this
- A **non-custodial Solana wallet** (we never hold the user's keys/funds; the user signs each payment, funds move peer-to-peer/program-to-merchant) sits **outside the card-network and money-transmission regimes that drive §2 entirely** — no PCI (no PANs), no network-token scoping, no PayFac registration, and (because we never custody) a strong argument against MTL. The cross-merchant "wallet" is *native*: one user wallet pays any merchant address.
- **The moment we custody** (hold user balances, run a hosted/custodial wallet, or operate fiat on/off-ramps) the MTL/BSA analysis comes roaring back, and crypto-specific state regimes (e.g. NY BitLicense) attach. So the sidestep is real **only while strictly non-custodial**. Treat custodial crypto as just another money-transmission path. (Inference grounded in the same MTL framework above; confirm with counsel — crypto money-transmission classification is actively contested.)

### Bottom line
Start with **Rank 1 (network-token / saved-PM reuse on Stripe Connect)** to get a Shop-Pay-style wallet for our Stripe-Connect merchants at the lowest regulatory cost, layering our own SMS-OTP consumer identity on top (exactly Shop Pay's pattern: central vault + identity + per-merchant charge). Keep **Solana non-custodial** as the zero-license parallel rail. Treat **NMI's per-merchant vault** as the part that won't cross-merchant cleanly — plan to consolidate those merchants onto the Connect wallet rather than trying to make NMI vaults shared. Escalate to **PFaaS (Rank 2)** only if/when we need PayFac economics and independence from Stripe's product roadmap. Avoid **MoR (Rank 3)** unless we truly become the seller, and avoid **full PayFac + MTLs (Rank 4)** until scale demands it.

---

## Sources
- Shop Pay vs Shopify Payments — merchantcostconsulting.com: https://merchantcostconsulting.com/lower-credit-card-processing-fees/shop-pay-vs-shopify-payments/
- What is Shop Pay — pagefly.io: https://pagefly.io/blogs/shopify/what-is-shop-pay
- Is Shop Pay Safe — adaptivpayments.com: https://adaptivpayments.com/blog/is-shop-pay-safe
- Shop Pay verification code — help.shop.app: https://help.shop.app/en/shop/shop-pay-settings/security/verification-code
- Shop Pay cross-store account behavior — community.shopify.com: https://community.shopify.com/t/can-i-enable-shop-pay-and-payments-without-the-remember-me-option-at-checkout/65752
- Payment tokenization / network tokens / PAR — shopify.com: https://www.shopify.com/blog/payment-tokenization
- Network tokenization (merchant-scoped) — paypal.com: https://www.paypal.com/us/brc/article/network-tokenization
- Shop Pay to non-Shopify merchants (FB/Google) — techcrunch.com: https://techcrunch.com/2021/06/15/shopify-expands-its-one-click-checkout-shop-pay-to-any-merchant-on-facebook-or-google/
- Shop Pay Commerce Component to external sites — siliconangle.com: https://siliconangle.com/2023/06/21/shopify-brings-shop-pay-checkout-service-external-e-commerce-websites/
- Shop Pay Component / commerce platforms — growpay.co: https://www.growpay.co/blog/products/article/shop-pay-can-now-be-integrated-with-other-commerce-platforms/
- Shopify MoR boundaries — dodopayments.com: https://dodopayments.com/blogs/shopify-merchant-of-record
- MoR for Shopify — gappgroup.com: https://gappgroup.com/blog/merchant-of-record-shopify/
- PayFac vs MoR — paddle.com: https://www.paddle.com/blog/payfac-vs-merchant-of-record
- Best MoR platforms — dodopayments.com: https://dodopayments.com/blogs/best-merchant-of-record-platforms
- FTC v. Paddle / MoR enforcement — mofo.com: https://www.mofo.com/resources/insights/250707-the-ftc-gives-the-merchant-of-record-model-a-paddling
- How to become a PayFac (sponsor bank, registration, PCI) — usio.com: https://usio.com/how-to-become-a-payment-facilitator/
- PayFac compliance — pxp.io: https://www.pxp.io/payments-glossary/payment-facilitator-compliance
- PayFac–acquirer relationship — infinicept.com: https://infinicept.com/payment-facilitator/learn/get-started/what-is-the-relationship-between-payment-facilitators-and-merchant-acquirers/
- PayFac requirements / sponsor banks — payram.com: https://www.payram.com/blog/how-to-become-a-payment-facilitator
- PayFac must-haves (PCI L1, BRAM) — datacapsystems.com: https://datacapsystems.com/blog/must-have-requirements-for-payfacs
- Agent-of-payee exemption map — csbs.org: https://www.csbs.org/agent-payee-exemption-map
- CA agent-of-payee guidance — dfpi.ca.gov: https://dfpi.ca.gov/wp-content/uploads/sites/337/2019/05/PRO-07-17-Electronic-Transactions-Association.pdf
- How money transmission laws work — moderntreasury.com: https://www.moderntreasury.com/journal/how-do-money-transmission-laws-work
- MTMA harmonization — cooley.com: https://www.cooley.com/news/insight/2024/2024-08-20-us-states-adopt-model-money-transmission-act-but-harmonization-remains-elusive
- MTL cost guide — brico.ai: https://www.brico.ai/post/how-much-do-mtls-cost
- MTL steps & requirements — innreg.com: https://www.innreg.com/blog/money-transmitter-license-steps-and-requirements
- Stripe cloning customers across accounts (legacy) — docs.stripe.com: https://docs.stripe.com/connect/cloning-customers-across-accounts
- Stripe direct charges on multiple accounts (current) — docs.stripe.com: https://docs.stripe.com/connect/direct-charges-multiple-accounts
- Alternatives to becoming a PayFac (Stripe Connect) — fiska.com: https://fiska.com/blog-articles/alternatives-becoming-a-payfac-for-saas/
- PayFac-as-a-Service providers — techbullion.com: https://techbullion.com/the-6-best-payfac-as-a-service-providers-for-platforms/
- PFaaS vs full PayFac — usio.com: https://usio.com/payfac-as-a-service-vs-full-payment-facilitation-a-guide/
