# Host-authorized customer routes

Draft implementation for the SaaS account extraction. This is a generic host
binding, not a customer-network service or an operational SaaS cutover.

The host authenticates the original request once and resolves its live authority
to one existing merchant/customer pair. The core keeps that original Verified
and Identity while binding the separate billing customer scope. It reuses the
ordinary customer handlers, views, payer rules and idempotency behavior. The
linked-customer sensitive-action declaration remains shared with the old network
mount; those operations ask the original credential for a recent sign-in. Reads
do not gain a step-up requirement.

No caller-controlled customer ID is admitted directly. A host resolver must prove
its own live account/link/merchant policy; an account reservation or old proof
record alone is not authority. Core validates the live merchant and existing
customer pair. The mapped mount does not provision customers, reassign their
issuer/tenant, enroll native network accounts or mount network-link routes.

The planned public seam is Engine.CustomerRoutes with Prefix, Auth and a trusted
Resolve(request, verified) callback returning a CustomerBinding. Its output is
normal public Route handlers, usable in-process or over HTTP. Prefix names the
customer root (default /v1/me), including trusted native whole-segment parameters.

Initial integration qualification covers customer order detail/next_action,
subscription detail/cancellation, owned-resource isolation, expiry/revocation,
original step-up and no double authentication. It uses real AuthKit/PostgreSQL
and existing public billing fixtures. Existing core network scenarios stay until
their SaaS replacements are qualified.

Before SaaS activation, remove the remaining legacy network DTO enrichment and
storage/lifecycle dependencies. In current core, GetCustomer/ListCustomers still
read billing.customer_links through runtime.Network; transferring that table
breaks those reads. UpdateCustomer returns settings without that enrichment.
These errors must not be hidden and no private billing-SQL workaround is allowed.
