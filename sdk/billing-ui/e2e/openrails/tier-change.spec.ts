// The client's change preview against real AuthKit + OpenRails. The
// seeded catalog declares no tier group, so every target is refused; jsdom
// tests cover the preview itself (src/client/catalog.test.ts).
import { expect, test } from "@playwright/test"

import { createBillingClient } from "../../src/client/client"
import { createUser, seedBilling } from "./api"

const billing = (baseURL: string | undefined, token?: string) =>
  createBillingClient({
    baseUrl: `${baseURL}/billing/v1`,
    getToken: () => token,
  })

test("change preview answers its owner and refuses the seeded targets", async ({
  request,
  baseURL,
}) => {
  const seeded = (await (await request.get("/__test/health")).json()) as {
    one_time_price_id: string
  }
  const owner = await createUser(request)
  const other = await createUser(request)
  await seedBilling(request, owner.id)
  const client = billing(baseURL, owner.access_token)
  const [sub] = (await client.listSubscriptions()).data
  const target = seeded.one_time_price_id

  await expect(client.previewSubscriptionChange(sub.id, { priceId: target })).rejects.toMatchObject({
    status: 400,
    code: "invalid_param",
    message: "cannot change to a different tier group",
  })
  await expect(
    client.previewSubscriptionChange(sub.id, { priceId: sub.price!.id })
  ).rejects.toMatchObject({ status: 409, message: "already on this plan" })
  await expect(
    billing(baseURL, other.access_token).previewSubscriptionChange(sub.id, {
      priceId: target,
    })
  ).rejects.toMatchObject({ status: 404 })
  await expect(
    billing(baseURL).previewSubscriptionChange(sub.id, { priceId: target })
  ).rejects.toMatchObject({ status: 401 })
})
