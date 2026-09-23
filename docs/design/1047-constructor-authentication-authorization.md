# Constructor authentication, authorization and customer routes

Status: proposed design, not a published API. Tracker: [1047](https://github.com/open-rails/tracker/blob/master/openrails/1047.md). This is separate from merchant bootstrap/publication1042, transport/demo1046, merchant Client selection design, and the frozen refund631 patch.

## Decision

OpenRails owns the operations, route registrations and their authorization requirements. A host supplies one provider-neutral authentication/authorization integration at construction. The optional AuthKit integration verifies through the host's existing verifier and evaluates native-user permissions against the live AuthKit Client. OpenRails does not infer permissions from role names, user JWT roles, a merchant slug or the fact that a caller is authenticated.

Declare HTTP policy in `embed.Options.HTTP`; remove public `Runtime.ConfigureHTTP`. Materialize standard routes after explicit merchant bootstrap, before serving requests. At that boundary, resolve an explicitly configured merchant slug to its immutable ID, validate that required authorities exist, then freeze the route configuration. Missing or conflicting configuration fails visibly; it does not select the first merchant or guess from a customer ID.

Slugs are lookup selectors. Immutable IDs remain in token subjects, stable API results and storage relationships. A reusable Client's per-operation merchant selector chooses the target; it does not grant authority. Do not add mutable `Client.SetMerchant` state.

## Current source and why it matters

- [`embed/embed.go`](../../embed/embed.go) already accepts `Options.HTTP`, but calls the public late-configurator internally. [`embed/http_routes.go`](../../embed/http_routes.go) freezes a separate mutable policy when routes are first materialized. The demo needs that late step only because its adapter requires a merchant UUID that construction just created.
- [`embed/authkit/authkit.go`](../../embed/authkit/authkit.go) accepts the host's actual `VerifyRequest` verifier, preserving issuer/audience and protocol verification. However, `NewDelegatedAuthenticator` defaults to `permissions.ForRoles(cl.Roles)`. The demo supplies an empty role mapper to turn that unwanted authority off. This default must go away.
- [`pkg/billingauth/gate.go`](../../pkg/billingauth/gate.go), [`authenticator.go`](../../pkg/billingauth/authenticator.go) and [`delegated.go`](../../pkg/billingauth/delegated.go) already separate neutral authentication, authorization and delegated identity. Consolidate their constructor wiring; do not introduce a second JWT implementation or a parallel self-service engine.
- [`internal/http/routes/routes.go`](../../internal/http/routes/routes.go) has separate native, service, delegated and trusted in-process branches. Non-user principals carry permission ceilings used for no-escalation checks. A new integration must preserve those branches' authority semantics, rather than reducing every credential to a user ID.
- [`internal/http/routes/self.go`](../../internal/http/routes/self.go) defines the actual self-service, restricted billing and subscription-only route sets. Payer ownership and customer-present payment checks are distinct. An invoker may read only its spend window; it cannot inherit the payer's account management.
- [`internal/controlplane/authority.go`](../../internal/controlplane/authority.go) resolves a merchant group, checks live `CanOnGroup`, then maps the same captured group UUID to the billing merchant. [`group_scope.go`](../../internal/controlplane/group_scope.go) preserves that immutable association for mutations. Retain this protection against slug reuse/renames.
- [`internal/requestauth`](../../internal/requestauth) memoizes verification within the request. Authentication followed by authorization must reuse the verified result: verifying a DPoP proof twice can consume the proof twice, and reparsing claims on another path can weaken admission.

## Neutral integration sketch

The exact exported names should be checked against the existing types before implementation. The essential boundary is two responsibilities with one constructor-owned integration:

```go
// Provider-neutral. These are conceptual signatures, not additional code today.
type Authentication interface {
    Authenticate(ctx context.Context, request *http.Request) (Identity, error)
}

type Authorization interface {
    Authorize(ctx context.Context, request *http.Request,
        identity Identity, requirement Requirement) error
}

type Integration struct {
    Authentication Authentication
    Authorization  Authorization
}
```

`Identity` contains the stable subject, issuer, an explicit canonical CustomerID, verified principal kind and interaction provenance. It is not a role or permission snapshot. Distinguish native user, machine/API key, delegated customer and trusted in-process host; retain invoker and credential restrictions. A string `UserID` being populated is not proof of a native user session.

`Requirement` identifies an OpenRails operation and its already resolved target: platform, merchant, or customer account, including immutable IDs and canonical slugs for context. The host authorizer does not choose another merchant while granting the operation. The engine checks that the authorized target is the one the handler executes against.

The AuthKit adapter keeps the original verified claims in its request-scoped memo, not in a serializable token-claims bag. Authorization must match the supplied identity to that memo and reject absent/inconsistent verification. Do not expose a generic opaque `any` field or globally cache identity by token. Host-provided implementations are trusted application code; request JSON cannot construct an authenticated identity.

Provider webhooks retain their provider signature/account verification, and capability-based checkout retains its own capability validation. The user authentication integration applies only where a route requires an application principal. A webhook-only/headless application does not need a user verifier; configuring user authentication must not put Stripe or other provider callbacks behind a user-JWT gate.

Authentication errors yield401, denied permission403, and an unavailable live authority503. Do not silently grant on lookup failure or downgrade an invalid credential to anonymous access on protected routes.

### AuthKit integration

```go
// Sketch: no role-name mapper and no merchant UUID at adapter construction.
auth, err := openrailsauthkit.New(openrailsauthkit.Config{
    Verifier: authRuntime.Verifier(),
    Client:   authClient,
    // Explicit mapping from billing target to AuthKit group/permission.
    MerchantAuthority: merchantAuthority,
    PlatformAuthority: platformAuthority,
    CustomerAuthority: sharedCustomerAuthority, // only for co-managed accounts
})
```

Ordinary fixed-merchant customer-only applications need just the verifier. Enabling privileged routes requires the live authority capability at construction. Do not add role mapping as an implicit fallback when `Client` or group mapping is missing.

For a native user, the adapter resolves the required AuthKit group and checks the concrete permission through `Client.Can`/`CanOnGroup`. Prefer an immutable group ID captured together with the billing target. Standard standalone merchant groups use their existing recorded group relationship. A host using its own group layout supplies an explicit target-to-group/permission mapping; that mapping is a selector, and `Can` remains the authority. No direct query to AuthKit tables and no privileged Runtime method is needed.

For cross-merchant platform operations, define explicit operation-specific platform permissions. A directory-read permission must not silently authorize refunds or credential edits. Merchant operation authorization can be satisfied by the merchant's current permission or an explicitly configured platform counterpart for that exact operation. There is no special role named `admin`. Root/owner behavior follows the actual AuthKit permission catalog, not a string comparison. The platform counterpart vocabulary and default enablement are a reviewed implementation decision; do not add an implicit universal bypass.

Preserve the host's optional explicit admission/liveness hook. The default native JWT path does not look up bans on each request; login/refresh enforce bans and current tokens can remain usable until expiry. Permission checks are nevertheless always live. Doujins/Hentai0's existing opt-in live admission remains configured during migration; the demo does not acquire it accidentally.

The standard customer adapter accepts native user credentials only. Machine/API-key/delegated credentials use the existing explicit credential path, with the issuer/audience/proof/revocation rules, controlling merchant/group/resource binding, permission ceilings and any invoker restrictions intact. Live authorization never widens a credential ceiling. A downscoped credential does not become unrestricted `/me` authority because it carries a user field.

## Reusable Client credential selection

The adjacent1048 design passes the immutable requested merchant slug/ID explicitly to a per-request credential provider. This matches the authorization boundary: it is requested target data, not proof of merchant existence or authority. The server resolves and authorizes independently; no anonymous preflight or broader-credential fallback is needed. Keep that callback within the existing Bearer/API-key mint path unless a typed credential contract explicitly preserves other schemes. A sender-constrained/DPoP credential must not lose its scheme, proof or ceiling through a string-token convenience. Requests on one shared Client must never mutate the Client's merchant, headers or credential source.

## Customer routes and ownership

One `CustomerRoutes` model replaces `Customer bool` and `CustomerExposures`. The ordinary entry uses the fixed `/v1/me` route group. The app's outer mount remains `/billing` (or another app-chosen prefix). Route sets are generated once and consumed identically by HTTP/Chi, Gin and Fiber.

```go
// Intended ordinary shape; final singular/list spelling is implementation work.
Auth: auth,
HTTP: &embed.HTTPConfig{
    CustomerRoutes: []embed.CustomerRoutesConfig{{
        Merchant: "openrails-demo", // explicit slug, resolved inside OpenRails
        Scope:    embed.CustomerBillingManagement,
    }},
},
```

No prefix or empty role mapper is needed for the built-in entry. `CustomerBillingManagement` retains history/access/payment-method/recovery and existing agreement management; it does not mount generic product checkout, change-tier or Stripe portal. The app continues to validate a selected post price and create its checkout through the Client.

Personal customer ownership is `target.CustomerID == verified.CustomerID` in the selected merchant. It requires no artificial customer role and no per-request group lookup. Resource queries still include both merchant and customer predicates. Co-managed treasury, a merchant paying as a customer, and delegated spend are different cases: they require an explicit payer resolver plus current authorization, not an equality shortcut.

An optional additional audience mount is justified by SaaS and uses the same route model/registrations. Its trusted resolver may map the verified actor to a different payer only after the appropriate live owner/account permission check. The resolver cannot silently alter the selected billing merchant. Additional prefixes are an advanced audience-mount feature, not required syntax for the ordinary built-in customer surface.

## Caller migration

| Caller | Current need | Migration |
|---|---|---|
| Demo | One platform merchant, native customer, restricted billing history; app-specific post checkout | Constructor integration with existing AuthKit verifier, explicit merchant slug, built-in billing-management profile; remove UUID extraction/role mapper/late ConfigureHTTP |
| Doujins | Fixed merchant; user checkout, own treasury, merchant administration; explicit live admission and live BillingInspect mapping | Declare routes/auth at construction; preserve own-customer treasury and explicit live policy; replace URL-sniffing permission expansion with per-operation live authority mapping |
| Hentai0 | Fixed merchant; same full customer/admin surfaces; native-session-only and live policy | Same constructor migration, retaining native-session discriminator and current permission authority |
| OpenRails-SaaS portal | Native user plus live selected-merchant access | One configured customer audience with explicit merchant/payer resolver and ordinary full profile |
| OpenRails-SaaS platform billing | Hosted merchant owner acts as hosted-merchant payer in the platform merchant's book | Additional subscription-only audience; live owned-merchant lookup remains, slug is payer selector, billing merchant stays platform; retain four library-owned routes and frontend calls |
| Standalone | Internal AuthKit/control-plane verifier and groups, service/API-key/delegated branches | Constructor declares standalone policy; infrastructure binds the built-in integration after control-plane creation and before routes; no late public policy setter |
| Tests/adapters | Many synthetic late ConfigureHTTP calls | Construct policy in fixtures; test copying/route freezing rather than competing late setters; all native adapters share standard path and authority tests |

Remove `Runtime.ConfigureHTTP`, old `Customer` boolean, `CustomerExposures`, role-name defaults and redundant default-authenticator fallbacks in the coordinated minor release. Preserve real privileged delegated APIs and explicit permission resolvers until their replacements are qualified; do not remove them merely because the demo no longer needs them. Do not publish no-op compatibility shims.

## Implementation gates and pitfalls

1. Qualify neutral contracts and the optional AuthKit integration with real signed native/delegated/API-key proofs. Verify exactly once per request; native role/permission strings never grant; live revoke affects the next privileged request; existing native JWT ban timing and explicit live opt-in remain as configured.
2. Test merchant/group slug rename or reuse races: one resolved immutable association must drive both authorization and mutation. Scope selectors supplied through body/header/path must agree where multiple assertions exist; cross-merchant selection cannot bypass credential binding.
3. Test personal ownership, co-managed treasury, customer-present payment action, delegated invoker-only access and cross-merchant platform operator separately. Never equate merchant owner, customer, payer and verified user.
4. Freeze constructor policy by copying slices/config values. Bind names after explicit merchant bootstrap, but before HTTP begins; no publicly mutable Runtime permission configuration. Unknown/retired merchants and missing authority fail visibly.
5. Test automatic HTTP/Gin/Fiber route catalogs and negative routes for restricted profiles. Retain caller rate-limit/CORS/body limits and original request path/proof verification through mounts.
6. Migrate current source consumers on coherent public versions, including standalone and all native adapter submodules. Keep refund631 release independent. Qualify the demo against the existing real sandbox transaction read-only; no duplicate provider charge/refund.

This design intentionally does not change reusable Client merchant-scoping semantics, credential storage/backend ownership, merchant configuration bootstrap, payment state ownership, or public stable IDs. Those are adjacent owner lanes. The small customer-auth prototype in the task worktree is exploratory and is not the final full integration API.

### Canonical customer identity

Neutral native identity requires `SubjectID` and `Issuer`. Customer, checkout and personal own-catalog
operations additionally require an explicit canonical UUID `CustomerID`; merchant
staff need no payable customer mapping. Hosts map `(Issuer, SubjectID)` to their payable customer UUID;
OpenRails has no issuer/opaque-subject mapping table and never hashes, guesses,
or rewrites an external subject. The optional AuthKit adapter sets `CustomerID`
from its verified local `UserID`. Two issuers with the same opaque subject can
therefore represent different payable customers. Missing mapping on a customer operation, or a supplied invalid mapping, is401.
The original subject and issuer remain the inputs for privileged authorization;
personal payer predicates use the canonical customer ID. Ownership does not grant
merchant refund or administration permission.

Native `merchant:catalog:own:*` operations use the explicit canonical identity as
the personal owner key, preventing equal opaque subjects from different issuers
from sharing one creator catalog. Missing mapping denies that personal operation;
ordinary staff/platform authorization continues to use the original issuer and
subject without requiring a payable customer. Explicit channel/group catalog
selection retains its separate permission and ownership checks.
