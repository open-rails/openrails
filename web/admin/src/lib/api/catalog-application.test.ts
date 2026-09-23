import { afterEach, expect, it, vi } from "vitest"
import { applyCatalog } from "./endpoints"
import { selectMerchant, server } from "@/test/harness"

afterEach(() => vi.unstubAllGlobals())

it("sends exact YAML with durable identity and int64 money unchanged", async () => {
  await server()
  selectMerchant("merchant-one")
  const raw = `schema_version: 1
application_id: authored-revision-7
expected_revision: 7
products:
  - key: premium
    prices:
      - key: premium-usd
        unit_amount: 9223372036854775807
`
  const fetcher = vi.fn(async () => Response.json({ replayed: false }))
  vi.stubGlobal("fetch", fetcher)
  await applyCatalog(raw)
  expect(fetcher).toHaveBeenCalledWith(
    "/v1/merchant/catalog/applications",
    expect.objectContaining({
      method: "POST",
      body: raw,
      headers: expect.objectContaining({
        "Content-Type": "application/yaml",
        "X-OpenRails-Merchant": "merchant-one",
      }),
    })
  )
})
