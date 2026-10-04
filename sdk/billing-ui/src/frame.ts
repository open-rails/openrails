// The frame protocol between an app's <CheckoutFrame> and the payment page's
// <CheckoutPage>. Both sides check the other's window and origin on every
// message; the page posts only to the origin recorded on its session.
import { z } from "zod"

import { checkoutSessionStatusSchema } from "./types"

const SOURCE = "openrails-checkout"

/** The page's color scheme, chosen by the framing app. */
export type CheckoutFrameTheme = "light" | "dark" | "auto"

const pageMessageSchema = z.discriminatedUnion("type", [
  z.object({ source: z.literal(SOURCE), type: z.literal("ready") }),
  z.object({
    source: z.literal(SOURCE),
    type: z.literal("resize"),
    height: z.number().finite().nonnegative(),
  }),
  z.object({
    source: z.literal(SOURCE),
    type: z.literal("complete"),
    status: checkoutSessionStatusSchema,
  }),
  z.object({
    source: z.literal(SOURCE),
    type: z.literal("redirect"),
    url: z.string(),
  }),
])
export type PageMessage = z.infer<typeof pageMessageSchema>

const appMessageSchema = z.object({
  source: z.literal(SOURCE),
  type: z.literal("init"),
  theme: z.enum(["light", "dark", "auto"]),
})
export type AppMessage = z.infer<typeof appMessageSchema>

type Body<M> = M extends unknown ? Omit<M, "source"> : never

export const pageMessage = (body: Body<PageMessage>): PageMessage =>
  ({ source: SOURCE, ...body }) as PageMessage

export const appMessage = (body: Body<AppMessage>): AppMessage =>
  ({ source: SOURCE, ...body }) as AppMessage

export function parsePageMessage(data: unknown): PageMessage | null {
  const parsed = pageMessageSchema.safeParse(data)
  return parsed.success ? parsed.data : null
}

export function parseAppMessage(data: unknown): AppMessage | null {
  const parsed = appMessageSchema.safeParse(data)
  return parsed.success ? parsed.data : null
}

/** The origin of a payment page URL, or null when it is not one. */
export function pageOrigin(url: string): string | null {
  try {
    const parsed = new URL(url)
    if (
      (parsed.protocol !== "https:" && parsed.protocol !== "http:") ||
      parsed.username ||
      parsed.password
    )
      return null
    return parsed.origin
  } catch {
    return null
  }
}
