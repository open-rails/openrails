# NMI host integration and pending checkout states

Owner: `/root/astra_neutral_resume`.
Worktree: `/home/fidika/cozy/.worktrees/billing-ui/1055-nmi-checkout-ui-20260923`.
Branch: `feat/1055-nmi-checkout-ui-20260923`.
Base: `05d6e27` from freshly fetched `origin/master`.

The demo will reuse this package's existing Collect.js hosted fields through a
custom CheckoutSource that invokes its admitted application checkout wrapper.
It will not enable generic billing checkout creation or send raw card data to the
application. Stripe remains available alongside the explicitly selected NMI PSP.
Only public tokenization configuration reaches the browser.

Bounded library changes:

- Represent `processing` as nonterminal server truth. Poll the same source for
  progress without repeating pay or tokenization, including after reload. Local
  quote expiry must not turn an accepted unresolved payment into a failed sale.
- Expose a tokenized-card form composed from the existing Collect.js driver and
  billing fields, for a host's separately consented save-card step. Do not model
  card setup as a zero-priced purchase or show a payment-success receipt for it.
- Retain exact string money and existing callbacks. Tests belong to this library;
  the demo uses build/lint and manual qualification only.

The existing metadata rename PR15 and customer billing package PR13 remain
separately owned. Publication uses the normal npm release workflow after review
and CI; no source checkout path ships in a host dependency manifest. Real NMI
iframe/tokenization/provider qualification remains a separate browser gate.
