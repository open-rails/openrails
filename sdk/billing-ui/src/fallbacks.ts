// A checkout's fallback PSPs: one kept back until the PSPs it stands in for
// are not offered, or have failed often enough on the order. The checkout
// decides this, never the merchant's PSP configuration.
import { z } from "zod"

import type { Order } from "./client/types"
import type { PspConfig } from "./psp"

/** A PSP kept back for others, named by key. */
export interface PaymentFallback {
  /** The PSP keys it stands in for, e.g. `["mobius"]`. */
  for: string[]
  /** Failed attempts through them on the order, combined, that reveal it. */
  afterFailures: number
}

/** Fallback PSPs by key: `{ ccbill: { for: ["mobius"], afterFailures: 2 } }`. */
export type PaymentFallbacks = Record<string, PaymentFallback>

type Option = Order["payment_options"][number]

/**
 * The psp_ids of the options a fallback keeps back: one of its `for` PSPs is
 * offered here and they have failed fewer than `afterFailures` times.
 */
export function hiddenFallbacks(
  fallbacks: PaymentFallbacks | undefined,
  options: Option[],
  psps: PspConfig[]
): Set<string> {
  const hidden = new Set<string>()
  if (!fallbacks) return hidden
  const keyOf = new Map(psps.map((psp) => [psp.psp_id, psp.key.toLowerCase()]))
  const named = new Map(
    Object.entries(fallbacks).map(([key, f]) => [key.toLowerCase(), f])
  )
  for (const option of options) {
    const fallback = named.get(keyOf.get(option.psp_id) ?? "")
    if (!fallback) continue
    const primaries = new Set(fallback.for.map((key) => key.toLowerCase()))
    const offered = options.filter((o) =>
      primaries.has(keyOf.get(o.psp_id) ?? "")
    )
    const failed = offered.reduce((n, o) => n + o.failed_attempts, 0)
    if (offered.length > 0 && failed < fallback.afterFailures)
      hidden.add(option.psp_id)
  }
  return hidden
}

// The pay page reads fallbacks from its URL fragment, after the secret:
// #pus_…&fallbacks=<JSON>. It accepts only small, well-formed values.
const FRAGMENT_PARAM = "fallbacks"
const MAX_FRAGMENT = 4096
const MAX_FALLBACKS_JSON = 1024
const MAX_ENTRIES = 8
const pspKey = z.string().regex(/^[a-z0-9][a-z0-9_-]{0,62}$/)
const fallbacksSchema = z
  .record(
    pspKey,
    z.strictObject({
      for: z.array(pspKey).min(1).max(MAX_ENTRIES),
      afterFailures: z.number().int().min(1).max(100),
    })
  )
  .refine((value) => {
    const n = Object.keys(value).length
    return n >= 1 && n <= MAX_ENTRIES
  })

/** The pay URL carrying fallbacks to the pay page in its fragment. */
export function withPaymentFallbacks(
  url: string,
  fallbacks?: PaymentFallbacks
): string {
  if (!fallbacks || Object.keys(fallbacks).length === 0) return url
  const at = url.indexOf("#")
  const base = at < 0 ? url : url.slice(0, at)
  const fragment = at < 0 ? "" : url.slice(at + 1)
  const param = `${FRAGMENT_PARAM}=${encodeURIComponent(JSON.stringify(fallbacks))}`
  return `${base}#${fragment ? `${fragment}&` : ""}${param}`
}

/** A pay URL fragment's secret and fallbacks; invalid fallbacks are dropped. */
export function readPaymentFragment(hash: string): {
  secret: string | null
  fallbacks?: PaymentFallbacks
} {
  const [secret = "", ...rest] = hash.replace(/^#/, "").split("&")
  const out = { secret: secret.startsWith("pus_") ? secret : null }
  if (hash.length > MAX_FRAGMENT) return out
  const raw = new URLSearchParams(rest.join("&")).get(FRAGMENT_PARAM)
  if (!raw || raw.length > MAX_FALLBACKS_JSON) return out
  try {
    const parsed = fallbacksSchema.safeParse(JSON.parse(raw))
    return parsed.success ? { ...out, fallbacks: parsed.data } : out
  } catch {
    return out
  }
}
