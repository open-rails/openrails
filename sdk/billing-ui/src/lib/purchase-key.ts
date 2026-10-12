// A purchase's Idempotency-Key, kept for the tab per customer and price (and a
// deposit's amount): a reload or a reopened dialog answers the same unpaid
// order, with a fresh checkout URL. It is released only once the order is paid
// or closed. Without storage it lives as long as the page.

/** Whose purchase of what a key belongs to; another customer never shares it. */
export interface PurchaseSlot {
  customer: string
  price: string
  amount?: string
}

const kept = new Map<string, string>()

const slotName = (slot: PurchaseSlot) =>
  `openrails.purchase:${JSON.stringify([slot.customer, slot.price, slot.amount ?? null])}`

/** The slot's key: the one kept for it, else a new one. */
export function purchaseKey(slot: PurchaseSlot): {
  key: string
  resumed: boolean
} {
  const name = slotName(slot)
  let key = kept.get(name)
  try {
    key ??= sessionStorage.getItem(name) ?? undefined
  } catch {
    // storage disabled
  }
  const resumed = key !== undefined
  key ??= crypto.randomUUID()
  kept.set(name, key)
  try {
    sessionStorage.setItem(name, key)
  } catch {
    // kept for this page
  }
  return { key, resumed }
}

/** Frees the slot once its order is paid or closed. */
export function releasePurchaseKey(slot: PurchaseSlot): void {
  const name = slotName(slot)
  kept.delete(name)
  try {
    sessionStorage.removeItem(name)
  } catch {
    // storage disabled
  }
}
