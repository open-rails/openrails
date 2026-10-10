// CheckoutSource is the flow's only data dependency: how to read the session
// and how to pay it. client.checkoutSource(id) uses OpenRails' session routes,
// by default with the session id as the only credential; tests and previews
// inject fixtures through the same interface.
import type { CheckoutSession, PayRequest, PayResult } from "./types"

export interface CheckoutSource {
  getSession(): Promise<CheckoutSession>
  pay(request: PayRequest): Promise<PayResult>
}
