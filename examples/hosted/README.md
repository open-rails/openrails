# Hosted SDK sample

Use the [OpenRails SaaS developer guide](../../docs/saas.md) for current merchant
setup, credentials, and hosted checkout. This sample illustrates a remote Go
client, customer-token handling, catalog application, and entitlement checks in
a course store.

The sample sends the same service token to the billing API and a SCIM directory.
Its `OPENRAILS_DIRECTORY` setting assumes a deployment that grants directory
access to that credential. The SaaS guide describes the current developer setup.
