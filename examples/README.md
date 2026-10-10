# Examples

One app three ways: a course site where users sign in with AuthKit, buy a
course, rent one for 3 days, buy the bundle, or join the channel membership,
which unlocks every video. Each folder is complete on its own; copy the one
you need. They share no code by import, and `shared_test.go` keeps the parts
they have in common identical: the catalog, the content routes and media
signing (`content.go`), and the React pages (`web/src/pages.tsx`, `main.tsx`)
with their browser tests (`web/e2e`).

| | [`embedded`](embedded) | [`standalone`](standalone) | [`hosted`](hosted) |
|---|---|---|---|
| **OpenRails runs** | in the app's process | as the `openrails` server you run | on the hosted platform |
| **You operate** | the app and PostgreSQL | the app, the server, PostgreSQL 18, Redis and SMTP | the app |
| **Billing client** | `openrails.New(ctx, cfg, deps)` | `openrails.NewRemote(url, WithTokenProvider(…), WithDefaultMerchant(…))` | `openrails.NewRemote(apiHost, WithAPIKey(…), WithDefaultMerchant(…))` |
| **Backend credential** | none: in process | client credentials from the app's AuthKit (`courses-backend`) | the console's service token |
| **Browser's billing calls** | same origin, `/billing/v1`, with the AuthKit session token | the server, with a DPoP-bound `openrails:self` token from AuthKit token exchange (`courses-web`) | the merchant's API host, the same way |
| **Who trusts the app's AuthKit** | nothing to trust: `Mount` takes `ak.Authenticator()` | the merchant's `remote_application` in `merchant.yaml`, a trusted issuer of the server's AuthKit | the merchant's registered signing application |
| **Catalog** | `Config.Catalog`, applied by `New` | `openrails apply-catalog` | `Client.ApplyCatalog` at boot, with the service token |
| **PSP** | `merchant.yaml`, read by `New` | `openrails/merchant.yaml`, read by the server at boot | added in the console |
| **Customer contacts** | `Deps.UserInfo`: AuthKit asked on each read | AuthKit pushes users over SCIM to the server's AuthKit (`/directory/scim/v2`) with its client credentials | the same push to the platform's directory, with the service token |
| **Receipts** | `Config.SMTP` (unset here) | the server's `email_smtp` | the platform sends them |
| **PSP webhooks** | `/billing/v1/webhooks/{rail}/{account_id}` on the app | `/v1/webhooks/{rail}/{account_id}` on the server | the URL the console shows for each PSP |
| **Route groups** | `Routes.RouteGroups`, with the app's permissions | the server's `route_groups` | on |

Only `main.go`, `web/src/clients.ts` and each folder's run files differ.
OpenRails emails a customer only at an address they proved, so the
standalone and hosted apps make AuthKit verify each sign-up's email. Each
folder's README says how to run it and its tests.
