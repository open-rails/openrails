import { createBillingClient, tokenSource } from "@openrails/billing-ui/client"

type Client = ReturnType<typeof createBillingClient>
export interface Fixture {
  alice: string
  bob: string
  token: string
  otherToken: string
  expiredToken: string
  price: string
  subscriptionPrice: string
  upgradePrice: string
  psp: string
  networkToken?: string
  networkMerchant?: string
  networkCustomer?: string
  networkAccount?: string
  nmiToken?: string
  nmiPSP?: string
  nmiCardToken?: string
}
declare global {
  interface Window {
    fixture: Fixture
    billing: Client
    calls: string[]
    makeBilling: (token?: string, fault?: string, baseUrl?: string) => Client
    makeRefreshingBilling: () => Client
    refreshes: number
    fault: string
  }
}
const fixture = (await (await fetch("/__fixture/config")).json()) as Fixture
window.fixture = fixture
window.calls = []
window.refreshes = 0
window.fault = ""
const observe = (client: Client): Client =>
  new Proxy(client, {
    get(target, key, receiver) {
      const value = Reflect.get(target, key, receiver) as unknown
      if (typeof value !== "function") return value
      return (...args: unknown[]) => {
        window.calls.push(String(key))
        return Reflect.apply(value, target, args)
      }
    },
  })
window.makeBilling = (token = fixture.token, fault = "", baseUrl = "/v1") =>
  observe(
    createBillingClient({
      baseUrl,
      getToken: () => token,
      fetch: (input, init) => {
        const headers = new Headers(init.headers)
        if (fault || window.fault)
          headers.set("X-Critical-Fault", fault || window.fault)
        return fetch(input, { ...init, headers })
      },
    })
  )
window.makeRefreshingBilling = () =>
  observe(
    createBillingClient({
      baseUrl: "/v1",
      getToken: tokenSource({
        skew: 0,
        fetchToken: async () => ({
          access_token:
            ++window.refreshes === 1 ? fixture.expiredToken : fixture.token,
          expires_in: 3600,
        }),
      }),
    })
  )
window.billing = window.makeBilling()
document.body.dataset.ready = "true"
