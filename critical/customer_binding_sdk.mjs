import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { pathToFileURL } from "node:url"

const input = JSON.parse(readFileSync(0, "utf8"))
const { createBillingClient } = await import(pathToFileURL(input.sdk).href)
const requests = []
const client = createBillingClient({
  baseUrl: input.base,
  getToken: () => input.token,
  fetch: (url, options) => {
    requests.push(url)
    return fetch(url, options)
  },
})
const order = await client.getOrder(input.order)
assert.equal(order.id, input.order)
assert.equal(order.customer_id, input.customer)
const page = await client.listOrders()
assert.deepEqual(page.data.map((item) => item.id), [input.order])
assert.ok(requests.every((url) => url.startsWith(`${input.base}/me/orders`)))
process.stdout.write(JSON.stringify({ requests, customer: order.customer_id }))
