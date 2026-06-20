# AuthKit / OpenRails Merchant Glossary

| Term | Meaning |
|---|---|
| OpenRails merchant | Billing/isolation namespace. It scopes subscriptions, payments, credits, usage, credentials, webhooks, and analytics. |
| AuthKit org | Ownership and control authority. Users, API keys, and remote applications receive permissions through org roles. |
| Remote application | AuthKit registered issuer/JWKS principal that can sign delegated access tokens, service JWTs, and remote application access tokens. It is a credential controlled by an org, not an OpenRails owner. |
| Delegated user | External OIDC subject from a registered issuer, carried as AuthKit `delegated_sub`. |
| Customer | OpenRails payable subject under a merchant. |
| Invoker | Principal that caused usage when it differs from the payable customer. |

## Standalone Chain

```text
JWT or API key -> AuthKit org -> OpenRails merchant
```

The OpenRails merchant stores `owner_org_id`. AuthKit decides which users,
API keys, and remote applications can act for that org. OpenRails then
checks whether the request is scoped to a merchant owned by that org.

## Delegated Browser Flow

1. Host backend signs a delegated access token with a registered remote
   application's issuer/JWKS.
2. OpenRails verifies `iss`, `aud`, `kid`, expiry, and delegated-token shape.
3. OpenRails resolves the issuer to its registered merchant.
4. OpenRails touches the customer `(merchant_id, delegated_sub)`; issuer is
   audit/last-seen source metadata only.
5. `/v1/me/*` routes target that authenticated subject. Browser-safe `org:`
   permissions gate delegated `/v1/admin/*` routes.

Tokens must not carry OpenRails merchant claims. The registered issuer is the
authority mapping.

## Service JWT Flow

First-party service JWTs use the same registered issuer/JWKS path, but carry
`token_use=service`, `jti`, a short lifetime, and server-to-server permissions.
OpenRails scopes the request to the issuer's merchant and any explicit resource
limits such as `openrails.customer=<customer_uuid>`.
