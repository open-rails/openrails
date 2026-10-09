import { afterEach, expect, it, vi } from "vitest"
import { PSPPublicationAttempts } from "@/lib/psp-publication"
import { adminMutations } from "@/lib/mutations"
import { client, exec, selectMerchant, server } from "@/test/harness"

afterEach(() => vi.unstubAllGlobals())

it("sends a stable operation and the reviewed revision on manual retry, without rebasing a conflict", async () => {
  const requests = await server({
    "PATCH /admin/psps/psp_a": Response.json(
      { code: "credential_operation_conflict", message: "conflict" },
      { status: 409 }
    ),
  })
  selectMerchant("merchant-a")
  const cache = client()
  const attempts = new PSPPublicationAttempts()
  const credentials = { secret_key: "sk_test_write_only" }
  const first = await attempts.prepare(["merchant-a", "psp_a"], { expected_revision: 7, credentials })
  const options = adminMutations.updatePSP(cache)
  await expect(exec(cache, options, { id: "psp_a", psp: first })).rejects.toThrow()
  const retry = await attempts.prepare(["merchant-a", "psp_a"], { expected_revision: 7, credentials })
  await expect(exec(cache, options, { id: "psp_a", psp: retry })).rejects.toThrow()
  expect(requests).toHaveLength(2)
  expect(first.operation_id).toMatch(/^[a-f\d-]{36}$/)
  expect(requests.map((request) => request.body)).toEqual([first, first])
  expect(first.expected_revision).toBe(7)
  expect(options.retry).toBe(false)
  expect(options.gcTime).toBe(0)
  cache.clear()
})

it("changes identity only for a changed intent or a completed operation, and isolates PSP and merchant", async () => {
  const attempts = new PSPPublicationAttempts()
  const body = {
    key: "main",
    rail: "stripe",
    account_id: "acct_a",
    credentials: { secret_key: "sk_test_a", webhook_signing_secret: "whsec_a" },
  }
  const first = await attempts.prepare(["merchant-a", "stripe"], body)
  const reordered = await attempts.prepare(["merchant-a", "stripe"], {
    ...body,
    credentials: { webhook_signing_secret: "whsec_a", secret_key: "sk_test_a" },
  })
  expect(reordered.operation_id).toBe(first.operation_id)
  for (const candidate of [
    await attempts.prepare(["merchant-b", "stripe"], body),
    await attempts.prepare(["merchant-a", "stripe"], { ...body, account_id: "acct_b" }),
    await attempts.prepare(["merchant-a", "stripe"], { ...body, key: "other" }),
    await attempts.prepare(["merchant-a", "stripe"], {
      ...body,
      credentials: { secret_key: "sk_test_changed" },
    }),
  ])
    expect(candidate.operation_id).not.toBe(first.operation_id)
  attempts.complete(first.operation_id)
  expect((await attempts.prepare(["merchant-a", "stripe"], body)).operation_id).not.toBe(
    first.operation_id
  )
})

it("refuses dispatch after the selected merchant changes", async () => {
  const requests = await server()
  selectMerchant("merchant-a")
  const cache = client()
  const options = adminMutations.createPSP(cache)
  selectMerchant("merchant-b")
  await expect(
    exec(cache, options, {
      key: "main",
      rail: "stripe",
      account_id: "acct_a",
      operation_id: crypto.randomUUID(),
    })
  ).rejects.toThrow("Merchant changed")
  expect(requests).toHaveLength(0)
  cache.clear()
})
