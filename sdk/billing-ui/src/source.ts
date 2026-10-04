// CheckoutSource is the flow's only data dependency: how to read the session
// and how to pay it. client.checkoutSource(id) targets OpenRails' session
// routes with the session id as the sole credential; tests and previews
// inject fixtures through the same interface.
import type { CheckoutSession, PayRequest, PayResult } from "./types"

export interface CheckoutSource {
  getSession(): Promise<CheckoutSession>
  pay(request: PayRequest): Promise<PayResult>
}
