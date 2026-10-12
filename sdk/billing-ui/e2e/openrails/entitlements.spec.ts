// The client's entitlements read against real AuthKit + OpenRails: the seeded
// subscription's product grants its key.
import { expect, test } from "@playwright/test"

import { createBillingClient } from "../../src/client/client"
import { createUser, seedBilling } from "./api"

const billing = (baseURL: string | undefined, token?: string) =>
  createBillingClient({
    baseUrl: `${baseURL}/billing/v1`,
    getToken: () => token,
  })

test("a customer reads the entitlement keys they hold", async ({
  request,
  baseURL,
}) => {
  const owner = await createUser(request)
  const other = await createUser(request)
  await seedBilling(request, owner.id)

  expect(await billing(baseURL, owner.access_token).getEntitlements()).toEqual({
    data: [
      { customer_id: owner.id, entitlement: "e2e-membership", quantity: null },
    ],
    next_cursor: null,
  })
  expect(
    await billing(baseURL, owner.access_token).getEntitlements({
      prefix: "other-",
    })
  ).toEqual({ data: [], next_cursor: null })
  expect(
    await billing(baseURL, other.access_token).getEntitlements()
  ).toEqual({ data: [], next_cursor: null })
  await expect(billing(baseURL).getEntitlements()).rejects.toMatchObject({
    status: 401,
  })
})
