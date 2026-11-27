# Examples

A course store sold three ways, plus two backends. Each folder is complete on
its own and its README says how to run it.

| Example | Shows | Guide |
|---|---|---|
| [`embedded`](embedded) | OpenRails in the app's Go process, behind its AuthKit | [Embedded](../docs/embedded.md) |
| [`standalone`](standalone) | the same store calling the `openrails` server it runs (Compose) | [Standalone](../docs/standalone.md) |
| [`hosted`](hosted) | the same store calling the hosted platform | [Client: credentials](../docs/client.md#credentials) |
| [`own-auth`](own-auth) | two merchants on one engine behind the host's own users, sessions and roles | [Embedded: auth](../docs/embedded.md#auth) |
| [`gate`](gate) | an API whose requests count toward usage limits, admitted in process by `openrails.Gate` | [Client: the Gate](../docs/client.md#limits-and-the-gate) |
| [`guide`](guide) | the fragments the guides show that no app runs; they compile | [Integration guide](../docs/guide.md) |

The three stores share `content.go`, `web/src/pages.tsx` and the catalog; only
`main.go`, `web/src/clients.ts` and the run files differ.

While no account holds a root role, each store's AuthKit logs a single-use `/setup#token=…` link on the store's origin at start; its `/setup` page renders auth-ui's `OwnerSetup`, which opens it and creates the owner account.
