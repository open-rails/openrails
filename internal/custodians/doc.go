// Package custodians is the registry of custodian kinds. A custodian holds a
// card; a rail charges it. The axes are orthogonal: payment_methods.rail says
// who charges, payment_methods.custodian who holds (psp, basis_theory,
// hyperswitch, platform; never empty, enforced by a CHECK). psp_id is set
// exactly when custodian = 'psp' (payment_methods_psp_custody_check); a
// custodian-held card belongs to no PSP and each charge routes to the one live
// PSP of its rail that reaches the custodian (charge.RoutePSP, else
// ErrNoRoute). CCBill and Solana produce no payment_methods row: no row means
// no stored instrument. charge_via (pan_proxy, network_token) is how the
// credential is presented; payments.token_type records what went out.
//
// Everything a kind needs, its secret slots, settings, the rails that can
// charge its instruments and how a browser tokenizes against it, is a
// Descriptor here, so a new vendor is a descriptor plus its seams, never a
// branch in the validator, reconciler, browser projection or webhook router.
// Adding a kind also takes a migration and a models.Custodians constant, so an
// unknown custodian cannot reach a money path.
//
// A merchant declares custodians.<key> {kind, account_id, ...} once and PSPs
// reference it with custodian: <key>. Only rails the descriptor names may
// (today nmi, the one detokenizing-proxy path); an undeclared key, a live/test
// mismatch, or a custodial PSP that also declares a tokenization key is
// refused. Secrets are scoped by identity:
// custodians/<kind>/<environment>/<account_id>/<key>.
//
// Settings card_vault (where new cards are saved: a PSP's own vault, its
// custodian, or platform) and charge_psps (which PSPs charge new cards) must
// agree: a PSP's own vault charges only through that account, a custodian's
// card through any PSP naming it, a platform card through NMI PSPs without
// card_entry: server. Violations are refused for that merchant alone
// (card_vault_unknown, charge_psp_unknown, charge_psp_incompatible,
// platform_vault_unavailable, platform_vault_unreachable). Changing either
// moves no card; renewals charge through the PSP that holds the agreement.
//
// The platform vault is deployment configuration (Config.CardVault): a Basis
// Theory tenant whose credential document lives at
// <kv_mount>/<scope_prefix>/platform_vault/<tenant_id>, holding the private
// api_key, per-merchant public collection applications and the account
// updater switch. A merchant using it gets a billing.custodians row with the
// reserved key platform and account <tenant_id>/<merchant_id>; cards live in
// container /merchants/<merchant UUID>/, and the tenant and creator are
// checked before every charge, conversion and removal. Provision one public
// collection application per merchant with only token-intent creation; the
// private one needs token-intent and token reads, token creation, the proxy
// and token:delete. Missing credentials or a shared application fail closed.
//
// account_updater arms Basis Theory's real-time Account Updater (a priced
// add-on; the key needs account-updater:real-time:invoke). A card is checked
// ahead of a renewal inside account_updater_lookahead_days (default 14) and
// after an expired or reissued decline; results become card versions, nothing
// is deleted or canceled. A refused key raises card.account_updater.unauthorized
// and holds the vault's checks; an unappliable answer raises
// card.account_updater.unapplied.
package custodians
