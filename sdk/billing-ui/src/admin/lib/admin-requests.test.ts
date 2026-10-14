import { expect, it } from "vitest"
import { createAdminClient } from "./api/client"

it("sends an authorized admin write once without a separate recent-sign-in flow", async () => {
  const requests: RequestInit[] = []
  const client = createAdminClient({
    baseUrl: "/billing/v1",
    auth: {
      async authFetch(_url, init = {}) {
        requests.push(init)
        return Response.json({ id: "grant" })
      },
    },
  })
  const body = {
    items: [{ customer_id: "customer", product_id: "product", hours: 24 }],
  }
  await expect(
    client.api("/admin/product-access", { method: "POST", body })
  ).resolves.toEqual({ id: "grant" })
  expect(requests).toHaveLength(1)
  expect(requests[0].body).toBe(JSON.stringify(body))
})

it("does not replay a write or sign the host out after an authentication refusal", async () => {
  let requests = 0
  const client = createAdminClient({
    baseUrl: "/billing/v1",
    auth: {
      async authFetch() {
        requests++
        return Response.json(
          {
            error: {
              code: "credential_expired",
              message: "Sign in again",
              type: "authentication_error",
            },
          },
          { status: 401 }
        )
      },
    },
  })
  await expect(
    client.api("/admin/product-access", { method: "POST" })
  ).rejects.toMatchObject({ status: 401, code: "credential_expired" })
  expect(requests).toBe(1)
})

it("names a server that couldn't be reached or didn't answer", async () => {
  const client = (authFetch: () => Promise<Response>) =>
    createAdminClient({ baseUrl: "/billing/v1", auth: { authFetch } })
  await expect(
    client(async () => {
      throw new TypeError("Failed to fetch")
    }).api("/admin/orders")
  ).rejects.toMatchObject({
    status: 0,
    message: "Couldn't reach the server. Check your connection and try again.",
  })
  await expect(
    client(async () => new Response("<html>", { status: 502 })).api(
      "/admin/orders"
    )
  ).rejects.toMatchObject({
    status: 502,
    message:
      "The server isn't responding right now. Please try again in a moment.",
  })
  await expect(
    client(async () =>
      Response.json(
        { error: { code: "service_unavailable", message: "Billing paused" } },
        { status: 503 }
      )
    ).api("/admin/orders")
  ).rejects.toMatchObject({ status: 503, message: "Billing paused" })
})
