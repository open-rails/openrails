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
const invoice = await client.getInvoice(input.invoice)
assert.equal(invoice.id, input.invoice)
assert.equal(invoice.customer_id, input.customer)
const page = await client.listInvoices()
assert.deepEqual(page.data.map((item) => item.id), [input.invoice])
assert.ok(requests.every((url) => url.startsWith(`${input.base}/me/invoices`)))
process.stdout.write(JSON.stringify({ requests, customer: invoice.customer_id }))
