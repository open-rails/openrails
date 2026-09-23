import { expect, test } from "@playwright/test"

import { api, createUser, seedBilling } from "./api"

const me = "/billing/v1/me"

type Page<T> = { data: T[] }
type Subscription = {
  id: string
  status: string
  cancel_scheduled: boolean
  resumable: boolean
  cancel_mode: string
  card: { brand: string; last4: string } | null
}
type State = Pick<Subscription, "status" | "cancel_scheduled" | "resumable">

test("seeded customer lists and manages their own billing", async ({
  page,
  request,
}) => {
  const user = await createUser(request)
  await seedBilling(request, user.id)
  await page.goto("/")
  const token = user.access_token

  const subs = await api(page, "GET", `${me}/subscriptions`, token)
  expect(subs.status, JSON.stringify(subs.body)).toBe(200)
  const [sub] = (subs.body as Page<Subscription>).data
  expect(sub).toMatchObject({
    status: "active",
    cancel_scheduled: false,
    card: { brand: "visa", last4: "4242" },
  })

  const methods = await api(page, "GET", `${me}/payment-methods`, token)
  expect(methods.status, JSON.stringify(methods.body)).toBe(200)
  expect((methods.body as Page<unknown>).data).toEqual([
    expect.objectContaining({
      id: expect.stringMatching(/^pm_/),
      type: "card",
      card: { brand: "visa", last4: "4242" },
    }),
  ])

  const payments = await api(page, "GET", `${me}/payments`, token)
  expect(payments.status, JSON.stringify(payments.body)).toBe(200)
  expect((payments.body as Page<unknown>).data).toEqual([
    expect.objectContaining({
      status: "succeeded",
      amount: "9990000",
      currency: "USD",
      subscription_id: sub.id,
    }),
  ])

  const state = async (): Promise<State> => {
    const r = await api(page, "GET", `${me}/subscriptions/${sub.id}`, token)
    const { status, cancel_scheduled, resumable } = r.body as Subscription
    return { status, cancel_scheduled, resumable }
  }
  const cancel = await api(
    page,
    "POST",
    `${me}/subscriptions/${sub.id}/cancel`,
    token,
    { feedback: "e2e cancel" }
  )
  expect(cancel.status, JSON.stringify(cancel.body)).toBe(202)
  await expect
    .poll(state)
    .toEqual({ status: "cancelled", cancel_scheduled: true, resumable: true })

  const resume = await api(
    page,
    "POST",
    `${me}/subscriptions/${sub.id}/resume`,
    token
  )
  expect(resume.status, JSON.stringify(resume.body)).toBe(202)
  await expect
    .poll(state)
    .toEqual({ status: "active", cancel_scheduled: false, resumable: false })
})

test("customer routes reject anonymous and foreign access", async ({
  page,
  request,
}) => {
  const owner = await createUser(request)
  const other = await createUser(request)
  await seedBilling(request, owner.id)
  await page.goto("/")

  const anonymous = await page.evaluate(async (path) => {
    const res = await fetch(path)
    return res.status
  }, `${me}/subscriptions`)
  expect(anonymous).toBe(401)

  const own = await api(page, "GET", `${me}/subscriptions`, owner.access_token)
  const [sub] = (own.body as Page<Subscription>).data
  const foreign = await api(
    page,
    "GET",
    `${me}/subscriptions/${sub.id}`,
    other.access_token
  )
  expect(foreign.status).toBe(404)
  const empty = await api(
    page,
    "GET",
    `${me}/subscriptions`,
    other.access_token
  )
  expect((empty.body as Page<Subscription>).data).toHaveLength(0)
})
