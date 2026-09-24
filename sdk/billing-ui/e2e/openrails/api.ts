import { expect, type APIRequestContext, type Page } from "@playwright/test"

export type Reply = { status: number; body: unknown }

// Browser-side same-origin fetch, as the billing-ui client will issue it.
export function api(
  page: Page,
  method: string,
  path: string,
  token: string,
  body?: unknown
): Promise<Reply> {
  return page.evaluate(
    async ({ method, path, body, token }) => {
      const headers: Record<string, string> = {
        Authorization: `Bearer ${token}`,
      }
      if (body !== undefined) headers["Content-Type"] = "application/json"
      const res = await fetch(path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      })
      const text = await res.text()
      return { status: res.status, body: text ? JSON.parse(text) : null }
    },
    { method, path, body, token }
  )
}

export type TestUser = { id: string; email: string; access_token: string }

export async function createUser(request: APIRequestContext) {
  const res = await request.post("/__test/users")
  expect(res.status(), await res.text()).toBe(201)
  return (await res.json()) as TestUser
}

export async function seedBilling(request: APIRequestContext, userId: string) {
  const res = await request.post(`/__test/users/${userId}/billing`)
  expect(res.status(), await res.text()).toBe(201)
  return res.json()
}
