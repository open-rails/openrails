/// <reference types="node" />
// @vitest-environment jsdom
import { webcrypto } from "node:crypto"
import { QueryClientProvider } from "@tanstack/react-query"
import { afterEach, expect, it, vi } from "vitest"
import type { PaymentProviderConfig } from "@/lib/api/types"
import { client, selectMerchant, server } from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"
import { RotateCredentialsDialog } from "./index"

afterEach(unmount)

it("the mounted rotation form submits reviewed revision, reuses its operation after conflict, and clears secrets on success", async () => {
  browserEnvironment()
  vi.stubGlobal("crypto", webcrypto)
  let succeed = false
  const requests = await server({
    "PUT /merchant/payment-providers/stripe": () =>
      succeed
        ? { payment_provider: {} }
        : Response.json(
            {
              error: {
                code: "credential_operation_conflict",
                message: "Changed",
              },
            },
            { status: 409 }
          ),
  })
  selectMerchant("merchant-a")
  const cache = client()
  const provider: PaymentProviderConfig = {
    id: "psp_a",
    rail: "stripe",
    account_id: "acct_a",
    configuration_revision: 7,
    credentials: { secret_key: { configured: true } },
    environment: "test",
    archived: false,
    drained: false,
    open_obligations: 0,
    first_seen_at: "2026-09-23",
    created_at: "2026-09-23",
    updated_at: "2026-09-23",
  }
  await mount(
    <QueryClientProvider client={cache}>
      <RotateCredentialsDialog
        provider={provider}
        credentialKeys={["secret_key"]}
      />
    </QueryClientProvider>
  )
  await click("Rotate")
  await act(async () => {
    const input = document.querySelector<HTMLInputElement>(
      'input[type="password"]'
    )!
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, "sk_test_input")
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
  await click("Validate & rotate")
  await vi.waitFor(() => expect(requests).toHaveLength(1))
  const first = requests[0].body as Record<string, unknown>
  expect(first).toMatchObject({
    account_id: "acct_a",
    expected_revision: 7,
    credentials: { secret_key: "sk_test_input" },
  })
  expect(first.operation_id).toMatch(/^[a-f\d-]{36}$/)
  // A refreshed object cannot silently rebase the already submitted intent.
  provider.configuration_revision = 12
  succeed = true
  await click("Validate & rotate")
  await vi.waitFor(() => expect(requests).toHaveLength(2))
  expect(requests[1].body).toEqual(first)
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
  await click("Rotate")
  expect(
    document.querySelector<HTMLInputElement>('input[type="password"]')!.value
  ).toBe("")
  await vi.waitFor(() => expect(cache.getMutationCache().getAll()).toHaveLength(0))
  cache.clear()
})
