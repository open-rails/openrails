import { afterEach, expect, it, vi } from "vitest"
import { ProviderPublicationAttempts } from "@/lib/provider-publication"
import { adminMutations } from "@/lib/mutations"
import { client, exec, selectMerchant, server } from "@/test/harness"

afterEach(() => vi.unstubAllGlobals())

it("sends stable operation and reviewed revision on manual retry, without rebasing a conflict", async () => {
  const requests = await server({
    "PUT /merchant/payment-providers/stripe": Response.json(
      { code: "credential_operation_conflict", message: "conflict" },
      { status: 409 }
    ),
  })
  selectMerchant("merchant-a")
  const cache = client()
  const attempts = new ProviderPublicationAttempts()
  const body = {
    account_id: "acct_a",
    credentials: { secret_key: "sk_test_write_only" },
  }
  const first = await attempts.prepare("merchant-a", "stripe", 7, body)
  const options = adminMutations.savePaymentProvider(cache)
  await expect(
    exec(cache, options, { rail: "stripe", provider: first })
  ).rejects.toThrow()
  const retry = await attempts.prepare("merchant-a", "stripe", 99, body)
  await expect(
    exec(cache, options, { rail: "stripe", provider: retry })
  ).rejects.toThrow()
  expect(requests).toHaveLength(2)
  expect(first.operation_id).toMatch(/^[a-f\d-]{36}$/)
  expect(requests.map((request) => request.body)).toEqual([first, first])
  expect(first.expected_revision).toBe(7)
  expect(options.retry).toBe(false)
  expect(options.gcTime).toBe(0)
  cache.clear()
})

it("changes identity only for a changed intent or completed operation and isolates account and merchant", async () => {
  const attempts = new ProviderPublicationAttempts()
  const body = {
    account_id: "acct_a",
    credentials: { secret_key: "sk_test_a", webhook_signing_secret: "whsec_a" },
  }
  const first = await attempts.prepare("merchant-a", "stripe", 2, body)
  const reordered = await attempts.prepare("merchant-a", "stripe", 3, {
    ...body,
    credentials: { webhook_signing_secret: "whsec_a", secret_key: "sk_test_a" },
  })
  expect(reordered.operation_id).toBe(first.operation_id)
  expect(reordered.expected_revision).toBe(2)
  for (const candidate of [
    await attempts.prepare("merchant-b", "stripe", 2, body),
    await attempts.prepare("merchant-a", "stripe", 0, {
      ...body,
      account_id: "acct_b",
    }),
    await attempts.prepare("merchant-a", "stripe", 3, {
      ...body,
      credentials: { secret_key: "sk_test_changed" },
    }),
  ])
    expect(candidate.operation_id).not.toBe(first.operation_id)
  attempts.complete(first.operation_id)
  expect(
    (await attempts.prepare("merchant-a", "stripe", 4, body)).operation_id
  ).not.toBe(first.operation_id)
})

it("refuses dispatch after the selected merchant changes", async () => {
  const requests = await server()
  selectMerchant("merchant-a")
  const cache = client()
  const options = adminMutations.savePaymentProvider(cache)
  selectMerchant("merchant-b")
  await expect(
    exec(cache, options, {
      rail: "stripe",
      provider: {
        account_id: "acct_a",
        operation_id: crypto.randomUUID(),
        expected_revision: 0,
      },
    })
  ).rejects.toThrow("Merchant changed")
  expect(requests).toHaveLength(0)
  cache.clear()
})
