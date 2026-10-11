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

The default browser URL is `https://billing.example.com/admin`; JSON requests use that origin's `/v1` routes.

The standalone server's app is in `server/web/console`. It owns local or trusted-issuer sign-in and renders `BillingAdmin` from the public package. `admin_console.enabled`, `admin_console.path`, and the existing sign-in settings configure its mount. No host extension bundle is built into it; a host composes its own app with the component.

`task admin-build` builds billing-ui and the standalone app into `server/web/console/dist`. `task build` embeds that output into the server binary. Release archives and Docker images build it before compiling Go. A Go build without frontend output still compiles, and attempting to enable a console without assets refuses startup.

For a supplied build, pass an `fs.FS` rooted at `index.html` as `server.Deps.ConsoleAssets`. Its generated index contains the mount placeholder `<base href="/admin/">`, rewritten by the server when serving a different console path.
