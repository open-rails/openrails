# OpenRails SaaS Composition Boundary

OpenRails core remains a multi-merchant billing engine. A hosted SaaS product composes it as a library and owns the hosted product behavior.

## Core Owns

- Billing runtime, workers, merchant-scoped API handlers, catalog, checkout, credits, subscriptions, entitlements, and provider webhook verification.
- Private standalone provisioning through CLI/bootstrap.
- AuthKit control-plane construction with private posture by default.
- Exported embedded seams for host auth, delegated browser principals, control-plane attach, route mounting, workers, and webhook handlers.
- Merchant terminology in code and APIs.

## SaaS Host Owns

- Public registration and onboarding.
- Platform-operator console.
- Host-to-merchant webhook resolution.
- Tenant membership UI and product onboarding.
- Meta-billing for the hosted OpenRails product.
- Tenant terminology in product copy.

## Embedded Seams

- Runtime: `pkg/embedded.New`, `Embedded.App`, `Embedded.RunWorkers`, `Embedded.Service`.
- HTTP: `Embedded.NewHTTPHandler` for default merchant-scoped billing routes; `pkg/embedded/gin` for gin hosts.
- AuthKit: `pkg/embedded/controlplane.AttachWithOptions` with `HostedPosture` for public registration and full AuthKit routes.
- Platform authority: `platform:superadmin` gates directory/platform routes; SaaS seeds the platform org and never grants it to tenant admins.
- Webhooks: default wiring mounts only `/merchants/:merchant/webhooks/:provider`; hosted SaaS mounts `pkg/embedded/gin.RegisterHostWebhookRoutes` behind Host-resolver middleware that pins `merchant.ID` before OpenRails verifies the merchant's signing secret.

No `saas_mode` flag exists in core. The deployed binary and host-injected seams choose the posture.
