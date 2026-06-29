# AuthKit / OpenRails Merchant Glossary

| Term | Meaning |
|---|---|
| OpenRails merchant | Billing/isolation namespace. It scopes subscriptions, payments, credits, usage, credentials, webhooks, and analytics. |
| Merchant permission-group | Ownership and control authority for one OpenRails merchant. Users, API keys, and remote applications receive permissions through group roles. |
| Remote application | AuthKit registered issuer/JWKS principal that can sign delegated access tokens, service JWTs, and remote application access tokens. It is a credential nested under a permission-group, not an OpenRails owner. |
| Delegated user | External OIDC subject from a registered issuer, carried as AuthKit `delegated_sub`. |
| Customer | OpenRails payable subject under a merchant. |
| Invoker | Principal that caused usage when it differs from the payable customer. |

## Standalone Chain

```text
JWT or API key -> merchant permission-group -> OpenRails merchant
```

The OpenRails merchant stores `permission_group_id`. AuthKit decides which users,
API keys, and remote applications can act for that permission-group. OpenRails
then checks whether the request maps to the merchant linked to that group.

## Delegated Browser Flow

1. Host backend signs a delegated access token with a registered remote
   application's issuer/JWKS.
2. OpenRails verifies `iss`, `aud`, `kid`, expiry, and delegated-token shape.
3. OpenRails resolves the issuer to its registered merchant.
4. OpenRails touches the customer `(merchant_id, delegated_sub)`; issuer is
   audit/last-seen source metadata only.
5. `/v1/me/*` routes target that authenticated subject. Merchant permissions
   gate delegated `/v1/merchant/*` routes.

Tokens must not carry OpenRails merchant claims. The registered issuer is the
authority mapping.

## Service JWT Flow

First-party service JWTs use the same registered issuer/JWKS path, but carry
`token_use=service`, `jti`, a short lifetime, and server-to-server permissions.
OpenRails scopes the request to the issuer's merchant and any explicit resource
limits such as `openrails.customer=<customer_uuid>`.
