# OpenRails Billing Engine

You're a developer, or an AI agent, who's tasked with adding payments to your app. Well you've come to the right place. You see 

Add payments to your application in a matter of hours or days, not months. OpenRails is perfect for you if you're building:

- SaaS products: Define your tier-list of plans customers can buy, and then bill them per seat. Your customers can upgrade/downgrade their plan with proration.
- Adult sites: Your users buy recurring subscriptions; we handle the subscription-lifecycle (prevent duplicate subscriptions, cancellation, manual-dunning when rebill fails, etc.), and we act as the source of truth of which users have access to what (entitlements). Build your own OnlyFans, Pornhub, Patreon, etc.
- Digital Storefronts: Your users buy videos / courses / downloads individually. Define your own products + prices; we manage ownership-records. Build your own Shopify, Gumroad, etc.
- API Platform or Cloud: Your users top-up their API balance ahead of time, and then you drawdown that balance as requests arrive. Or you enable arrears-billing for your users, and keep a ledger of their usage and compile them into regularly monthly invoices.

OpenRails supports many payment-processors:

- Any processor using an NMI-payment gateway. Examples: PaymentCloud, PayKings, SoarPay, Zen Payments, etc. These processors are just white-labels on top of NMI's gateway.
- CCBill
- Solana with USDC (crypto)
- Stripe

OpenRails can be run as a standalone server, or as an embedded process within your Golang webserver. In the future, we will be making available a hosted-version, so you don't have to manage infrastructure.

---

### How It Works

Operator Side (you, the merchant):
- Define your catalog: products, the entitlements granted by products, and prices
- 

Customer Side (your users):
- Exposes a set of routes for creating a purchase

---

### Features

- **Checkout sessions** — one unified session model across every rail: provider-hosted
  redirect (Stripe, CCBill), tokenized-vault card entry (NMI Collect.js), and Solana Pay.
- **Catalog as source of truth** — declare your products, prices, rate cards, and tiers
  once; OpenRails pushes them to providers (find-or-create) and watches for drift.
- **Entitlements & product ownership** — your webserver asks one question ("does user X
  have Y right now?") against your own database, never a provider API.
- **Tiered plans with proration** — users upgrade/downgrade between tiers; preview the
  proration before committing.
- **Dunning: capture lost revenue** — failed rebills are retried on a schedule derived
  from the billing cycle, with a staleness window that guarantees a months-old failure is
  cancelled, never surprise-charged.
- **Payment-lifecycle emails, handled** — "your payment failed", "your card expires
  soon", renewal and cancellation notices: templated, deduplicated (a resolved failure
  supersedes the stale notice instead of double-sending), and branded per merchant. You
  don't build the awkward-conversation system; we are the awkward-conversation system.
- **Higher approval rates, measurably** — network tokens, automatic account-updater
  refresh of expired/reissued cards before rebills, and correct stored-credential
  (MIT/CIT) framing on every recurring charge. Then prove the uplift in your own
  dashboard: approval rate is sliceable by token type, rail, and decline reason.
- **Metered billing & spend control** — pre-authorize with holds (admit → capture/release),
  per-user budgets, spend caps, trust levels, prepaid credits or arrears invoicing.
- **Solana, for real** — one-off USDC payments *and* on-chain recurring subscriptions via
  the official delegation program, devnet-tested like any other sandbox.
- **Browser-direct self-service** — end-users hit `/v1/me/*` (balance, invoices,
  subscriptions, payment methods, cancellation) straight from your frontend; no proxy
  routes to write or maintain.
- **Batch import of legacy data** — bring existing subscribers, payments, and vault
  references in from a previous billing system and let reconciliation converge them.
- **Metric alerts to your endpoints** — define alerts on any billing metric (approval
  rate drops, chargeback spikes, revenue anomalies) and receive them on outbound
  webhook sinks you configure.
- **Team & scoped API keys** — invite your staff with role-scoped permissions;
  machine access through API keys that carry explicit grants and can never cross
  merchants.
- **Merchant console** — an optional admin UI for your team: customers, subscriptions,
  refunds, catalog, API keys, and a metrics dashboard you can customize with your own
  key-metric widgets (plus an optional LLM analytics copilot).

---

### Who Uses OpenRails

Archetypes — the site you're building, and what OpenRails does for it:

- **Building an OnlyFans, Fansly, or Pornhub Premium** — recurring memberships on
  processors that will actually board adult content (NMI ISOs like MobiusPay/PaymentCloud,
  CCBill). You get duplicate-subscription prevention, dunning that chases failed rebills,
  and entitlements that survive lost webhooks — the subscription-lifecycle plumbing every
  paysite rebuilds badly.
- **Building a Patreon or Substack** — creator/membership tiers with upgrades,
  downgrades, and proration. Your app asks "is this person a patron at tier 2 right now?"
  against its own database, not a provider API.
- **Building a Gumroad, itch.io, or Teachable** — videos, courses, games, downloads sold
  individually. Every purchase is a permanent ownership record (entitlement) your server
  queries forever; one checkout model whether the buyer pays by card or USDC.
- **Building a Linear, Notion, or Figma** — classic SaaS seat billing: define the tier
  list once, customers self-serve upgrades with proration previews, Stripe rail today
  with an exit ramp built in.
- **Building an OpenAI-platform or Replicate** — the credits console: users pre-pay a
  balance (or run arrears with monthly invoices), you draw it down per request. The
  admission API pre-authorizes with holds so a runaway job stops *before* the balance
  goes negative — a real ledger, not a usage counter bolted to Stripe.
- **Building a Midjourney or Cursor** — subscription + metered hybrid: a base plan plus
  GPU/LLM usage, per-user and even per-agent spend budgets with trust levels. Built for
  AI products where the marginal cost of a request is real money.
- **Building a Helius-style paid service for crypto users** — your customers live
  on-chain; take USDC straight to a wallet you control, including recurring on-chain
  subscriptions. No processor exists in the loop to drop you.
- **Running an Aylo-style network of sites** — several brands, one self-hosted billing
  deployment, each site an isolated merchant with its own rails on shared
  infrastructure, isolation enforced by the database.
- **Anyone who watched the 2021 OnlyFans near-ban** — if you're in a category processors
  purge (content, crypto, CBD, gaming), the question isn't *if* you get a termination
  email, it's *when*. Integrate rail-agnostic now and that email becomes a config change
  — swap to a high-risk gateway over a weekend, keep your subscribers, keep your
  entitlements, lose nothing.

---

### Failed Payments Are a Revenue Line

Industry numbers: 5–14% of subscription payments fail, involuntary churn is 20–40% of all
churn (ProfitWell), and good recovery tooling wins back 30–60% of failed payments. For a
site doing $100K/mo, that's roughly $2–5K/mo of revenue that leaks or doesn't, depending
entirely on plumbing. Merchants pay $79–500/mo for standalone dunning tools — or 10–25%
of recovered revenue to success-fee vendors — to capture it.

OpenRails ships the whole recovery stack built in:

- **Fewer failures in the first place** — network tokens, account-updater card refresh
  before rebills, and correct MIT/CIT stored-credential framing lift issuer approval
  rates on the charges that matter most: the renewals.
- **Smart retries on the failures that remain** — hard declines (stolen card,
  do-not-honor) go terminal immediately instead of burning retries; soft declines get a
  cycle-derived schedule, and a months-stale failure is cancelled, never surprise-charged.
- **The customer conversation, handled** — templated, deduplicated payment-failure and
  card-expiry emails that supersede themselves when the problem resolves.
- **Proof it's working** — approval rate by rail, token type, and decline reason on your
  dashboard, with alerts to your endpoints when it drops.

---

### Technical Guarantees

The engineering underneath, and the invariants it enforces — this is what you'd otherwise
build yourself, badly, under deadline:

- **The Convergence Engine.** Billing systems rot by drifting from provider truth: a lost
  webhook here, a manual refund there. OpenRails treats the provider as the source of
  truth for money state and *continuously* re-converges against it — inline after every
  mutation, on a background sweep, and via watermarked provider reads. Every divergence
  becomes a classified finding on one of four planes (provider-observed truth, derived
  effects, lifecycle clocks, internal consistency); safe repairs apply automatically,
  judgment calls queue for a human and never auto-fire.
- **Rebilling, both ways.** Leave recurring billing to the provider's native engine
  (NMI/CCBill/Stripe plans — OpenRails observes and converges), or let OpenRails drive
  the renewal itself: engine-initiated stored-credential charges where *we* decide the
  amount and timing (scheduled reprices, vaulted-card rails), and the on-chain crank for
  Solana. Same subscription model either way.
- **Double-entry money, twice over.** Every money movement is a balanced double-entry
  ledger transaction — value is never created or destroyed, only moved. A separate grant
  ledger tracks credit lots (who was granted what, from which source), and the
  Convergence Engine cross-checks the two continuously: duplicates, broken source
  references, and unbalanced effects surface as findings instead of festering.
- **Audited entitlements.** No entitlement exists without provenance: every access window
  records its source event, grant, and lifecycle (granted → revoked, with reason), so
  "why does this user have premium?" is a query, not an investigation. Excess grants
  (e.g. access surviving a refund) are detected and flagged automatically.
- **Effectively-once provider writes.** Every outbound mutation is a durable intent
  before it's an HTTP call: idempotency-keyed, origin-tagged, account-fingerprinted, and
  drained by an executor that resolves ambiguous outcomes by *reading* the provider
  before any retry. A charge is never blind-retried; a credential swap can never fire a
  stale queue against the wrong account.
- **Sandbox that can't lie.** `test_mode=sandbox` doesn't trust configuration — it
  proves it: live Stripe keys refuse to boot, and every NMI account is probed with a
  card only a simulator would approve. No real money can move in a sandbox boot, in any
  environment.
- **Your users never pay for our malfunction.** Access is closed by *evidence* — a
  confirmed cancellation, a terminal decline, exhausted dunning — never by the clock
  alone. A lost webhook, a dead pipe, or stale data parks the subscription for
  verification with access intact. Terminal cancellation is the last resort, not the
  default.
- **Database-enforced merchant isolation.** Multi-merchant isolation isn't application
  code you have to trust — it's Postgres row-level security on an unprivileged role,
  with every merchant-scoped table behind a policy. A query without merchant context
  returns nothing, by construction.
- **Time is injected, and the build enforces it.** Every billing decision runs on an
  injectable clock, guarded by a lint that fails the build on naked `time.Now()` in
  business logic. That's why a year of renewals, dunning, and expiry can be
  fast-forwarded in an integration test — the billing engine is deterministic under
  time travel.
- **One client, structurally incapable of drift.** Embedded and standalone modes share
  one client implementation over one handler surface — the in-process transport
  dispatches into the same routes HTTP does. Parity between modes isn't tested into
  existence; there's nothing to diverge.
- **Work scales with activity, not accounts.** No routine job ever sweeps
  everything-on-file: due work is indexed, provider reads are watermarked, and
  per-merchant jobs fan out only for merchants with something to do. Ten thousand idle
  merchants cost roughly nothing.

---

### Manifesto

OpenRails ultimate goal is to break the parasitic monopoly Visa + MasterCard currently hold upon American's financial lives. This pain is especially acute for 'high risk' businesses (crypto, porn, gambling, CBD, etc.) who are banned from most payment providers. OpenRails makes it much easier to integrate with 'high risk payment gateways', which have really shitty APIs usually, and lack all of the dev-ex nicities you get with Stripe.

---

### License

Our license is very permissive; you can do anything you like with OpenRails and its sourcecode, except create a multi-tenant hosted service that competes with our own hosted-SaaS platform. This is the only restriction we impose, since that is our primary business model.

---

### Required Services

Standalone Mode:
- Postgres 18+ (can be shared with your webserver)
- A redis-compatible service (we recommend Garnet) (Optional for rate-limiting)

Embedded Mode:
- Same as above, plus your webserver has to be written in Go, since this is a Go-library after all.

---

## Collecting Credit Card Info + PCI-Compliance

OpenRails does not receive or store your user's credit card details; only your payment-process should. There are two payment flows that achieve this:

- **Redirect flow**: Browser -> payment provider's checkout page (ex. Stripe) -> user enters credit card details -> payment provider redirects back to your frontend. Behind the scenes Stripe webhook -> Your OpenRails server -> updates entitlements in your database.
- **Tokenized-vault flow**: Browser -> sends credit card details directly to your payment provider -> browser receives a token in response -> browser sends the token to OpenRails -> OpenRails sends the charge + token to the payment provider -> payment provider charges the card and returns the result to OpenRails -> OpenRails updates entitlements in your database.

Your webserver + OpenRails only need PCI-compliance SAQ-A, which is a self-assessment + annual questionnaire; if you're handlnig or storing credit-card information that would make you SQ-D, which requires a lot of work.

---

### When NOT to Use OpenRails

We'd rather you find this out here than three weeks in. Skip OpenRails if:

- **You're a simple, low-risk SaaS that fully trusts Stripe.** A handful of subscription
  tiers, no metered credits, no risk of being classified "high-risk"? Direct Stripe
  (Checkout + Billing + a few webhook handlers) is genuinely less work than running us.
  OpenRails earns its keep when you need what Stripe doesn't give you: a local source of
  truth for access (entitlement checks against your own Postgres — no Stripe call in the
  hot path, keeps working through a Stripe outage), pre-authorization/spend-control for
  metered workloads, or the ability to switch rails without a rewrite when a provider
  drops you.
- **You need polished invoicing, tax, and revenue recognition today.** That's Stripe
  Billing / Lago / Kill Bill territory; our invoicing is functional, not an accounting
  suite.
- **You can't run Postgres.** OpenRails is self-hosted infrastructure with a database as
  its source of truth. If you want zero ops, wait for our hosted version.

---

### OpenRails vs Alternatives

Three open-source projects get compared to us most often. They're good products that sit
at different layers — here's an honest map:

| | **OpenRails** | **OpenMeter** | **Lago** | **Kill Bill** |
|---|---|---|---|---|
| Core job | Full billing engine: payments, subscriptions, entitlements, ledger | Usage **metering** + entitlement pipeline | Usage-based **rating + invoicing** | Subscription billing **framework** |
| Moves money itself | ✅ four rails, incl. high-risk + crypto | ❌ delegates to Stripe | ❌ delegates to PSP integrations | via payment plugins you run |
| High-risk gateways (NMI ISOs, CCBill) | ✅ first-class | ❌ | ❌ | write your own Java plugin |
| Crypto (Solana/USDC, incl. on-chain recurring) | ✅ | ❌ | ❌ | ❌ |
| Entitlements as the access source of truth | ✅ timeline your app reads | ✅-ish (feature access checks) | ❌ | subscription-state only |
| Pre-auth spend control (hold → capture, budgets, trust levels) | ✅ | ❌ meters after the fact | ❌ prepaid credits, no holds | ❌ |
| Invoicing / coupons / tax maturity | functional, not an accounting suite | ❌ (Stripe's problem) | ✅ its core strength | ✅ mature |
| Dunning / failed-payment recovery | ✅ built-in doctrine | ❌ | retry logic | ✅ overdue system |
| Embeds as a library in your app | ✅ Go, in-process | ❌ | ❌ | ❌ |
| Stack you operate | one Go binary + Postgres (+ optional Redis) | Go service + event pipeline | Rails app + Postgres + Redis + workers | JVM + MySQL + plugin runtime |

**vs OpenMeter.** OpenMeter is a usage-metering pipeline: it ingests high-volume event
streams, aggregates them into meters in real time, does feature/entitlement checks, and
hands the results to Stripe for actual billing. If your problem is *"billions of usage
events, Stripe is fine as the money layer,"* OpenMeter is the stronger metering engine —
its ingestion architecture is built for volumes we don't target. But it never touches
money: no checkout, no dunning, no ledger, no rails. And metering is after-the-fact by
design — it can tell you what a customer used, not stop them before an expensive request
exceeds their balance. OpenRails' admission API (hold → capture/release, spend caps,
per-invoker budgets) exists precisely for that pre-authorization gap, and our
double-entry ledger — not a Stripe mirror — is the money record.

**vs Lago.** Lago is the open-source Stripe-Billing/Chargebee alternative: plans,
metering, rating, coupons, prepaid credits, and genuinely polished invoicing, with
charging delegated to PSP integrations (Stripe, Adyen, GoCardless, …). If your business
is invoice-centric B2B billing on mainstream processors, Lago is further along than we
are on the invoicing/accounting surface, and we say so above. What it doesn't give you:
rail independence (its PSP list won't board high-risk businesses), any crypto rail, an
entitlement timeline your app can gate features on, pre-auth spend control, or an
embedded mode — it's a Rails service you deploy and call. OpenRails is the payments +
access-control engine; Lago is the invoicing engine.

**vs Kill Bill.** Kill Bill is the closest functional overlap: a mature, battle-tested
subscription-billing framework with real dunning, catalog versioning, and a plugin
architecture that can, in principle, reach any gateway. Its cost is operational and
developmental weight: a JVM/MySQL stack, XML catalogs, and gateway support that means
*writing and maintaining a Java plugin* — for a high-risk ISO or a crypto rail, you're
building the integration yourself on their SPI. Kill Bill's entitlement API tracks
subscription state; it isn't a per-feature access timeline. OpenRails trades fifteen
years of framework generality for a sharper shape: one Go binary (or an import into your
own), rails that board high-risk merchants working out of the box, entitlements as the
thing your app reads, and a reconciliation engine that continuously converges your
database against provider truth.

---

### Quickstart

```bash
# Try the full stack locally (Postgres + Garnet + OpenRails, zero-config):
task docker-up
curl http://localhost:3053/health/ready

# Or add it to your Go app as a library:
go get github.com/open-rails/openrails
```

Then pick your integration path below.

---

### Integrate with an AI Agent

Using Claude Code, Codex, or another coding agent? Paste this prompt to have it do the
integration for you:

```text
Integrate OpenRails (github.com/open-rails/openrails) — a self-hostable
billing/payments engine — into this application.

First, fetch and read the agent integration guide; it has the decision tree,
the milestone plan, and links to every doc you need:
https://raw.githubusercontent.com/open-rails/openrails/master/docs/agent-integration.md

Before writing any code, confirm with me:
1. Mode — embedded (this app is Go; the engine runs in-process) or
   standalone (separate service; any language can call it).
2. Payment rails — NMI-backed gateway / Stripe / CCBill / Solana, and
   whether I already have sandbox + live credentials.
3. What we sell — subscriptions, one-time purchases, metered usage/credits.

Then follow the guide's milestone plan in order, verifying each step before
the next. All development runs against sandbox rails (test_mode=sandbox);
prove the full checkout → webhook → entitlement flow end-to-end before ever
touching live credentials.
```

The agent-facing guide itself lives at [docs/agent-integration.md](docs/agent-integration.md).

---

### Documentation

**Integrate** — for developers wiring OpenRails into an application:

- [Embedded integration (Go library)](docs/embedded-integration.md) — run the engine in-process: boot, migrations, declaring your merchant, mounting the billing routes on your server, calling the in-process client.
- [Standalone integration (service)](docs/standalone-integration.md) — deploy OpenRails as its own service: production config, first-run provisioning, API keys, the Go SDK and plain-HTTP integration.
- [Frontend integration](docs/frontend-integration.md) — the browser side: self-service routes, checkout flows (redirect, tokenized-vault, Solana), payment methods, tokens, and error handling.
- [The auth model](docs/auth.md) — one credential per trust domain: why embedded uses your session credential and standalone uses delegated tokens.
- [Batch import / legacy migration](docs/batch-import.md) — moving an existing subscriber base onto OpenRails: the import surface, the phased playbook, and the limited-mode cutover.
- [HTTP API reference](docs/api/endpoints.md) — every route, grouped by caller class.
- [Example: gated premium page](examples/gated-premium-page/) — a runnable standalone demo: public page, entitlement-gated `/premium`, delegated tokens, NMI tokenized-vault checkout.

**Payment rails** — per-rail setup: credentials, the manifest entry, webhooks, sandbox testing:

- [Certification matrix](docs/rails/certification-matrix.md) — which flows each rail supports, and what evidence backs each one
- [NMI](docs/rails/nmi.md) (MobiusPay, PaymentCloud, PayKings, and other NMI-backed ISOs)
- [Stripe](docs/rails/stripe.md)
- [CCBill](docs/rails/ccbill.md)
- [Solana](docs/rails/solana.md) (USDC, self-custody, on-chain recurring subscriptions)
- [Payment-method custody](docs/payment-method-custody.md) — who *holds* a stored card vs who *charges* it, and the combinations that are real today

**Run your business** — for the merchant defining plans and managing customers:

- [Merchant guide](docs/merchant-guide.md) — authoring the catalog (products, prices, rate cards, entitlements), pushing it, and day-to-day customer management.
- [Admin console](docs/admin-console.md) — the merchant portal: turning it on/off, building the assets, and using it.
- [Entitlements](docs/entitlements_timeline.md) — the access-timeline model your app reads.

**Operate the deployment** — for whoever keeps it running:

- [Operator guide](docs/operator-guide.md) — infrastructure requirements (Postgres, Redis/Garnet, Vault), what runs by itself, and the drift toolbox.
- [Operations manual](docs/operations.md) — the deep reference: operating modes and safety levers, dunning, reconciliation, the provider intent ledger, cutovers.
- [Merchant provisioning](docs/merchant-provisioning.md) — manifests, credentials, secrets, and API keys.
- [Self-hosting mode 1](docs/self-hosting-mode1.md) — the manifest-is-truth deployment shape.
- [Vault](docs/vault.md) — HashiCorp Vault setup and secret operations.
- [Rate limiting](docs/rate-limiting.md) — the built-in per-IP/per-user limits and captcha escalation.

**Reference**

- [Glossary](docs/glossary.md) — rails, PSPs, merchants, payers, and the rest of the vocabulary.
- [Metrics & query API](docs/metrics-for-llms.md) — analytics access for dashboards and LLM agents.
- [Contributing / hacking on OpenRails](docs/dev/README.md) — dev workflow, testing, local webhooks.
