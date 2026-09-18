import type { CreditGrant, CreditGrantInput } from "@/lib/api/credit-types"
import { amountFromInput } from "@/lib/format"

export function creditGrantInput(
  input: {
    amount: string
    decimals: number
    currency: string
    expires: string
    description: string
    sourceID: string
  },
  allowed: boolean,
  now = Date.now()
): CreditGrantInput {
  if (!allowed) throw new Error("Your role cannot grant credit.")
  const amount = amountFromInput(input.amount, input.decimals)
  if (amount === null || BigInt(amount) <= 0n)
    throw new Error(
      `Enter a positive amount with at most ${input.decimals} decimal places, within the supported range.`
    )
  const currency = input.currency.trim()
  if (!currency) throw new Error("Select a currency.")
  let expires: string | undefined
  if (input.expires) {
    const time = new Date(input.expires).getTime()
    if (!Number.isFinite(time) || time <= now)
      throw new Error("Expiry must be a valid future date.")
    expires = new Date(time).toISOString()
  }
  return {
    amount,
    currency,
    source: "admin",
    source_id: input.sourceID,
    expires_at: expires,
    description: input.description.trim() || undefined,
  }
}

export function canRevokeCredit(grant: CreditGrant, allowed: boolean): boolean {
  return (
    allowed &&
    (grant.state === "active" || grant.state === "scheduled") &&
    /^\d+$/.test(grant.remaining_amount) &&
    BigInt(grant.remaining_amount) > 0n
  )
}

export function creditRevokeInput(
  grant: CreditGrant,
  allowed: boolean,
  reason: string
) {
  if (!canRevokeCredit(grant, allowed))
    throw new Error("This grant cannot be revoked with your current access.")
  reason = reason.trim()
  if (!reason || reason.length > 500)
    throw new Error("Enter a reason of up to 500 characters.")
  return { grant: grant.id, reason }
}
