// Package server is standalone OpenRails: the engine (openrails.New)
// composed, like any host of the library, with its own AuthKit and the
// multi-merchant control plane, serving the standalone HTTP surface. The
// openrails binary (cmd/openrails) and hosted products build on it; a host
// embedding OpenRails for one merchant uses package openrails alone. It is
// its own module, released in lockstep with the root; install the binary from
// a release or the image, since its go.mod builds against the root's source.
//
// New migrates and mounts the server's AuthKit at its issuer's path and mounts
// the engine routes with AuthKit's Authenticator, each merchant's AuthKit
// group as its scope. The server fixes merchant roles (viewer, support,
// owner), the API-key prefix and audience, the resource's scopes, AuthKit's
// River schema, invitation-only local registration and, when unset, the
// engine's trusted proxies; a declared merchant's Issuer is refused. Deps.Auth
// with nil Postgres, Redis or Email reuses the engine's. Run serves Handler
// and the workers, Serve only the handler; a product with its own router
// mounts Routes and adds RiverJobs to its fleet before Start.
//
// No route registers a merchant, manages a team or mints a key: those are
// these Go methods and the server's AuthKit (teams, invitations and API keys
// live in the merchant's AuthKit group; a key acts at its merchant alone with
// one role, never beyond its minter's). A hosted product authenticates with
// AuthKit().Authenticator() and checks merchant permissions in MerchantScope.
//
// A merchant's UUID is its identity; its name is a lookup key, unique among
// live merchants. Naming governs renames: the former name stays an alias no
// one else may claim until it expires, and deleting or retiring releases
// names. MerchantCreation gates ProvisionMerchant for an owner: a verified
// email, FreeAllowance owned merchants, then a vaulted payment method
// (Deps.HasVaultedPaymentMethod); refusals are billing.ErrInvalidMerchantSlug,
// ErrMerchantSlugReserved, ErrMerchantCreationEmailUnverified,
// ErrMerchantCreationPaymentMethodRequired and ErrMerchantCreationRefused. A
// name already held answers that merchant with Created false: check the owner
// before treating it as a repair. Ownerless provisioning is an ungated
// operator act.
//
// DeleteMerchant is soft and RestoreMerchant undoes it. PurgeMerchant is
// permanent: it needs MerchantPurgeInventory's id, row count and confirmation
// phrase typed back with the kill switch on, and a job then deletes the
// merchant's Vault configuration. RetireUnusedMerchant tombstones a merchant
// with no activity at all and releases its name and AuthKit group;
// CompletePendingMerchantRetirements retries failed group releases. Nothing
// here revokes cards at a PSP or removes the grant log.
package server
