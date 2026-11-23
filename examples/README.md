# Integration examples

These samples sell access to courses with the Go client and the browser SDK.
Start with the guide for the deployment you plan to use. The README for each sample explains its prerequisites and release context.

| Integration | Sample | Guide |
| --- | --- | --- |
| OpenRails runs inside your Go app | [Embedded](embedded) | [Embedded guide](../docs/embedded.md) |
| You run OpenRails as an HTTP service | [Standalone Compose](standalone) | [Standalone guide](../docs/standalone.md) |
| You connect to the hosted platform | [Hosted sample with older provisioning](hosted) | [OpenRails SaaS guide](../docs/saas.md) |

The [own-auth sample](own-auth) shows two merchants on an embedded engine with
the host application's own users, sessions, and roles.
The [gate sample](gate) is a backend whose requests count toward usage limits:
`openrails.Gate` admits each one in process and reports what it used.
