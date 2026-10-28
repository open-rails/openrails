import type { Finding, FindingChange } from "#orck/client/generated/wire"

// A group is the open proposals of one PSP and one change kind: "14 missed
// charges" reviewed and approved together.
export interface ProposalGroup {
  key: string
  psp: string | null
  kind: string
  items: Finding[]
}

const KIND_LABELS: Record<string, string> = {
  record_charge: "missed charges",
  record_refund: "missed refunds",
  adopt_subscription: "renewals to adopt",
  correct_subscription: "subscription corrections",
  cancel_subscription: "subscriptions ended at the provider",
  update_card: "changed cards",
  revoke_access: "access revocations",
  cancel_provider_schedule: "provider schedules to cancel",
  collect_skipped_renewal: "skipped renewals to collect",
  create_subscription: "subscriptions to create",
  refund_charge_after_cancel: "refunds after a cancel",
}

export const kindLabel = (kind: string) => KIND_LABELS[kind] ?? kind

// kindOf is a proposal's change kind, or its finding type when it proposes
// no change (review only).
export const kindOf = (f: Finding) => f.change?.kind ?? f.finding_type

export function groupProposals(findings: Finding[]): ProposalGroup[] {
  const groups = new Map<string, ProposalGroup>()
  for (const f of findings) {
    const kind = kindOf(f)
    const key = `${f.psp_id ?? ""}\u001f${kind}`
    const group = groups.get(key) ?? { key, psp: f.psp_id, kind, items: [] }
    group.items.push(f)
    groups.set(key, group)
  }
  return [...groups.values()]
}

export type Effect = keyof FindingChange["effects"]

export const EFFECTS: Effect[] = [
  "ends_access",
  "moves_money",
  "provider_write",
]

// effective counts the proposals that end access, move money or write to a
// provider: approving them takes a typed confirmation of their count.
export function effective(findings: Finding[]) {
  return findings.filter((f) => EFFECTS.some((e) => f.change?.effects[e]))
    .length
}
