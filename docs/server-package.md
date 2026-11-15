# The server package

`github.com/open-rails/openrails/server` is the `openrails` binary's library: a
module of its own, released with the root under the same version. A product that
serves many merchants with OpenRails' own accounts (merchant sign-up, teams, API
keys, trusted issuers, fleet analytics) builds on it. No OpenRails route registers
a merchant, manages a team or mints a key: those are this package's Go methods and
the server's AuthKit, on which a hosted product builds its own routes.

```bash
go get github.com/open-rails/openrails/server@vX.Y.Z
```

Install the binary from a release or the image, not `go install`: the server
module builds against the root's source (the embedded payment page included), so
its `go.mod` keeps a replace.

```go
srv, err := server.New(ctx, server.Config{
    Engine: openrails.Config{ProviderWriteMode: openrails.ProviderWritesFull},
    Auth: authkit.Config{
        Token: authkit.TokenConfig{Issuer: "https://billing.example.com"},
        Keys:  authkit.KeysConfig{Path: "/vault/auth"},
    },
    LocalSignIn: true,
}, server.Deps{Engine: openrails.Deps{Postgres: pool, Redis: rdb}})
if err != nil { return err }
return srv.Run(ctx) // serves Addr and runs the workers until ctx ends
```

`server.New` composes what any host does: the engine (`openrails.New`); the
server's own AuthKit, migrated and mounted at its issuer's path; and the
multi-merchant control plane, Go methods only. Engine routes are mounted with
AuthKit's Authenticator, each merchant's AuthKit group as its scope.

| `server.Config` | Meaning |
|---|---|
| `Engine` | the engine's `openrails.Config`; a declared merchant's `Issuer` is refused |
| `Auth` | the server's `authkit.Config`, passed through (issuer, keys, registration, rate limits, `Resource`, `SignIn`). The server fixes merchant roles, the API-key prefix and audience, the resource's scopes, AuthKit's River schema and mount, invitation-only local registration, and the engine's trusted proxies when unset. Open self-registration needs `MerchantCreation` |
| `LocalSignIn` | serve sign-in to the server's own accounts |
| `Naming` | merchant rename policy (below) |
| `MerchantCreation` | the policy for merchants users create: reserved names, a pattern, a free allowance |
| `AdminConsole`, `ConsoleIssuer` | the console (on at `/admin` by default; `&server.AdminConsole{Disabled: true}` omits it) and the issuer it signs staff in at |
| `Addr`, `PrivateAddr` | where `Run` listens (`:3053`) and serves `PrivateHandler` (`/metrics`) |
| `DrainDelay`, `ShutdownTimeout`, `CheckoutOrigin` | shutdown timing and the payment page origin |

`server.Deps` holds the engine's `Deps` (`Engine`), AuthKit's (`Auth`; nil
Postgres, Redis and Email reuse the engine's), `ConsoleAssets` (a filesystem rooted
at `index.html`; nil uses the embedded build), `HasVaultedPaymentMethod` and
`CustomerAdmissionPolicy` ([auth](auth.md#host-customer-apis)). `Run` serves
`Handler` and the workers; `Serve` serves without them; a product with its own
router mounts `Routes` and composes `RiverJobs` into its fleet before
`Start(ctx, openrails.WithRiverClient(fleet))`.

## Methods

| Area | Methods |
|---|---|
| Merchants | `ProvisionMerchant`, `ListMerchants`, `GetMerchant`, `MerchantByName`, `RenameMerchant`, `DeleteMerchant` (soft), `RestoreMerchant`, `MerchantPurgeInventory`, `PurgeMerchant`, `ListActiveMerchantIDs`, `ListMerchantsForSubject`, `ListEvents` |
| Authority | `MerchantScope`, `ListUserMerchants`, `AuthKit`, `EnsureCustomerPermissionGroup`; `server.MerchantRoles`, `server.MerchantRole` |
| Operations | `ListWorkerHealth`, `UnlockAdminLockout`, `PrivateHandler`, `FleetAnalytics`, `FleetTimeseries`, `ListMerchantRetirementCandidates`, `RetireUnusedMerchant`, `CompletePendingMerchantRetirements`, `SubjectHasVaultedPaymentMethod` |

The CLI covers the operator's (`merchants`, `workers`, `admin-lockouts`). A hosted
product authenticates with `srv.AuthKit().Authenticator()` and asks `Can` with
`srv.MerchantScope(ctx, id)` for merchant permissions or the `RootScope` for root
ones. Teams, invitations and API keys are AuthKit's, in the merchant's group.

**API keys.** `ak.CreateAPIKey(ctx, who, iam.GroupByID(merchantID), …)` with a
merchant role (`server.MerchantRole`), revoked with `ak.RevokeAPIKey`; with local
sign-in the AuthKit API serves `POST /groups/{group_id}/api-keys`, which the
console uses. The secret is shown once; the prefix `openrails_st_<key_id>`
identifies it. A key acts at its merchant alone with one role (`viewer`,
`support`, `owner`, or a custom one, [roles](auth.md#permissions)); AuthKit refuses
a role beyond the minting identity's own.

## Merchant names

A merchant's UUID is its identity and owns everything; its name (`slug`) is a
lookup key that can change. With a control plane a merchant is bound for good to
one AuthKit group by UUID, and the group's AuthKit name is not the merchant's. A
name is unique among live merchants. A rename keeps the former name as an alias
that no one else can claim until it expires (the merchant may take it back);
deleting or retiring releases names and aliases. Name-bearing inputs (CLI
`--merchant`, a manifest) resolve once to the UUID; `--merchant id:<uuid>` skips
the name. Provisioning a released name creates a new merchant.

```yaml
naming:                    # server.Config.Naming; NAMING_* variables
  enabled: true
  rename_interval: 72h     # 0s: rename at once
  former_names:
    mode: finite           # or forever, immediate (no duration)
    duration: 2160h        # 90 days
```

The operator renames with `RenameMerchant` (`openrails merchants rename`); a
hosted product sets `RenameMerchantParams.ActorUserID` for an owner, adding the
interval, reserved names and pattern.

## Hosted merchant creation

```go
cfg := server.Config{
    Engine:      engine,
    Auth:        authkit.Config{Token: authkit.TokenConfig{Issuer: "https://api.my-brand.example"},
        Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen}},
    LocalSignIn: true,
    MerchantCreation: &server.MerchantCreationConfig{
        ReservedSlugs: []string{"my-brand"}, // plus billing.ReservedMerchantSlugs
        FreeAllowance: 2,                    // owned merchants before a card on file
    },
}
var srv *server.Server
deps := server.Deps{
    Engine: openrails.Deps{Postgres: pool},
    HasVaultedPaymentMethod: func(ctx context.Context, subject string) (bool, error) {
        return srv.SubjectHasVaultedPaymentMethod(ctx, platformMerchantID, subject)
    },
}
srv, err := server.New(ctx, cfg, deps)
```

Your registration route calls `ProvisionMerchant` with the user as `OwnerUserID`.
The policy requires a verified email, allows `FreeAllowance` owned merchants, and
beyond that a vaulted payment method (no charge). Refusals:
`billing.ErrInvalidMerchantSlug`, `ErrMerchantSlugReserved`,
`ErrMerchantCreationEmailUnverified`, `ErrMerchantCreationPaymentMethodRequired`,
`ErrMerchantCreationRefused`. A name already held answers that merchant with
`Created: false`: check the user owns its group before treating it as a repair.
Ownerless provisioning is an operator act and ungated (how a platform merchant
claims a reserved name). Registration rate limits are yours.

**Retiring unused merchants** returns never-used names to the pool; when to do it
is yours. `ListMerchantRetirementCandidates` pages live merchants created before a
cutoff, oldest first, excluding reserved names, each with `Used`.
`RetireUnusedMerchant(ctx, merchantID, groupID)` refuses any activity (customers,
payments, subscriptions, ledger accounts, PSPs, custodians, secrets, webhooks,
intents, undelivered events, catalog definitions) and otherwise commits an
irreversible tombstone that releases the name, then deletes that AuthKit group;
`CompletePendingMerchantRetirements` retries failed group releases.

## Deleting merchants

No route deletes a merchant. `DeleteMerchant` (`openrails merchants delete`) is
soft: off the lists, credentials resolve nothing, records kept, and
`RestoreMerchant` brings it back. A purge is permanent:
`MerchantPurgeInventory` records what would go, and `PurgeMerchant` deletes it once
the inventory id, row count and confirmation phrase are typed back with the kill
switch on; a job then deletes its Vault configuration. No deletion revokes cards
at the PSP or removes the append-only grant log. Moving a merchant is
[portability](merchant-portability.md); recovering a deployment is
[backup](operations.md#backup-and-recovery).
