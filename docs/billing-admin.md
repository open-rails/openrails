# Billing administration in a host app

`BillingAdmin` supplies the billing pages inside a host's existing React app. The host owns sign-in and passes its authenticated transport. Embedded Go engines serve the billing API; the standalone server builds the same component into its console.

```tsx
import { BillingAdmin } from "@openrails/billing-ui/admin"
import "@openrails/billing-ui/admin.css"

<BillingAdmin
  baseUrl="/billing/v1"
  auth={auth}
  basePath="/admin/billing"
  navigation={{ pathname, search, navigate }}
  appearance={{ theme: "inherit" }}
/>
```

`auth` supplies `authFetch`; AuthKit's AuthClient and IssuerClient implement that interface. BillingAdmin neither stores tokens nor calls AuthKit's account API. Mount the component only for the signed-in person, and remount it when that identity changes. Each instance owns its request transport and query cache. `queryClient` optionally supplies an existing TanStack Query 5 client.

`navigation` is the host router's current `pathname`, `search`, and `navigate(to, { replace? })`. Links are relative to `basePath`. Omitting it uses browser history and listens to back/forward navigation. No nested React Router is created.

Billing writes use the staff member's permissions without a separate recent-sign-in flow or automatic replay. The host owns authentication, DPoP, and its choice of 2FA policy. AuthKit's own credential and role-management components keep their own step-up checks.

The browser page is distinct from the JSON API: `GET /v1/metrics/dashboard` reads the saved dashboard layout and widget definitions; `POST /v1/metrics/query` returns chart data.

The component reads `GET /v1/admin/access` for permissions and assistant availability. Hidden tabs do not grant or revoke authority: the API checks every request. The standalone server can pass `exchange: { resource, scope }`; its authenticated transport must then implement `resourceFetch`, and the selected merchant's id is the token's group.

## Composition

- `merchant` fixes the merchant, as an id or `{ id, slug, display_name? }`. `merchants` and `onMerchantChange` supply a switcher.
- `settingsTabs` adds `{ value, title, element }` entries. A host renders its own team or key management there.
- `customerPanels(customer)` adds host content to a customer page; `userHref(id)` links to the host's user page.
- `newMerchantHref` and `emptyState` let the host supply its onboarding.
- `language` sets the language of the component; `messages` overrides English messages by their English key. `billingAdminMessages` lists the English message bundle.
- `appearance` defaults to `theme: "inherit"`; light, dark, and host variable overrides are available. Styles and dialog portals remain inside the admin scope. Import the separate `admin.css` stylesheet.

`useBillingAdminAccess({ baseUrl, auth, merchant?, exchange? })` reads permissions outside the component so the host can gate its Billing tab. It returns `data`, `error`, `isPending`, and `refetch`.

`BillingCustomerSummary` takes the same connection props plus `customer` and an optional `href`. It displays balances, active subscriptions, entitlements, product access, and recent payments. Its lists are bounded previews; the link opens the full billing history.

`billingConfirmWarnings({ subscriptions })` supplies warning text for a host's ban or account-deletion confirmation. A ban or account deletion does not cancel subscriptions. `billingPermissionHelp` describes the billing and application permissions for the host's role editor.

The customer entry (`@openrails/billing-ui`) imports no admin modules or admin stylesheet. TanStack Query 5 is an optional peer used by the admin entry.

## Standalone server console

The standalone browser entry is `/admin`. For a server at `https://billing.doujins.com`, open `https://billing.doujins.com/admin`; its JSON APIs remain under `/v1`. An embedded host chooses the component's `basePath` itself.

The app in `server/web/console` composes three tabs from the public packages:

| Tab | Surface | Authority |
| --- | --- | --- |
| Billing | `BillingAdmin` | The selected merchant's billing access |
| Access | AuthKit's `MembersAdmin`, `RolesAdmin`, `ApiKeysAdmin`, and `RemoteApplicationsAdmin` | The selected merchant's group or the root group; each panel checks its own permissions |
| Account | AuthKit's password, second-factor, passkey and session panels | The locally signed-in person's own account |

App-signed-in staff manage people, roles and account security in their app. The console explains this instead of sending their remote tokens to the server's local AuthKit account API. Local owners and operators manage API keys and backend signing keys. There is no site-wide user browser or merchant registration page.

The authenticated `GET /admin/merchants` directory supplies the merchant selector. Root staff see live merchants; other staff see the merchants their verified credential permits. A name in a query or browser state grants nothing, and token exchange refuses an unauthorized merchant.

Declare app sign-in with `admin_console.issuers`, keyed by a merchant id or name (`root` for operators):

```yaml
admin_console:
  issuers:
    example-merchant:
      url: https://app.example.com/auth
      client_id: billing-console
      name: Example app
# local_sign_in: true
```

Register that public client at the app's authorization server with the callback `https://billing.example.com/admin/callback`. The app must already be trusted in the merchant's AuthKit group, and its role bounds the permissions or mapped roles in each token. AuthKit verifies resource, issuer, scope, claims and sender proof. The browser uses authorization code with PKCE.

`local_sign_in` separately offers local accounts. The authority manifest seeds the first operator; `openrails push-auth-bootstrap -f …` reapplies it. Everyone else joins by invitation. AuthKit credential creation and role grants retain native recent-sign-in challenges; billing operations use their permissions directly.

The console is always served at `/admin`. `admin_console.enabled` and `admin_console.path` are refused with migration instructions; singular `admin_console.issuer` is replaced by the keyed map. With neither app nor local sign-in configured, the page explains how to declare one and the server still boots.

`task admin-build` builds billing-ui and the standalone app into `server/web/console/dist`; release archives and Docker images embed it. A source-only Go build without frontend assets still starts, and the console answers 503 with the build instruction. A malformed supplied build remains a startup error. Pass a supplied build as `server.Deps.ConsoleAssets`, rooted at `index.html` with the build's `<base href="/admin/">` placeholder.
