// Payments → Health, NMI history: the queries it sends, how it shapes NMI's
// monthly history and marks OpenRails' own start. Fixtures mirror the e2e
// scenario: one-offs refused 1 in 3, NMI rebills 1 in 2, and a card
// verification in OpenRails' first recorded month.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { MetricsQuery, MetricsResult } from "@/lib/api/metrics"
import type { PSP } from "@/lib/api/types"
import { adminQueries } from "@/lib/queries"
import { formatBucket } from "@/pages/dashboard/lib"
import { client, render, selectMerchant, server } from "@/test/harness"
import fixtures from "./fixtures/nmi-history.json"
import { PaymentHealthPage } from "./health"
import {
  nmiHistoryMonths,
  nmiHistoryQueries,
  nmiRefusals,
  recordingSince,
  type Scope,
} from "./health-model"

type Captured = { query: MetricsQuery; result: MetricsResult }
const captured = fixtures.metrics as unknown as Record<string, Captured>
const result = (name: string) => captured[name].result

const all: Scope = { owner: "", last: "30d" }
const wire = (q: MetricsQuery) => JSON.parse(JSON.stringify(q))
const OCT = "2022-10-01T00:00:00Z"

beforeEach(async () => {
  await server()
  selectMerchant("merchant-a")
})
afterEach(() => vi.unstubAllGlobals())

describe("queries", () => {
  it("sends the captured queries over the 25 months kept", () => {
    const built = nmiHistoryQueries(all) as Record<string, MetricsQuery>
    expect(Object.keys(built).sort()).toEqual(Object.keys(captured).sort())
    for (const [name, { query }] of Object.entries(captured)) {
      expect(built[name].range, name).toEqual({ last: "25m" })
      expect(wire({ ...built[name], range: query.range }), name).toEqual(query)
    }
  })

  it("scopes to the PSP but not the owner NMI's history lacks", () => {
    const scope: Scope = {
      owner: "nmi_schedule",
      psp: { id: "psp_1" },
      last: "7d",
    }
    for (const [name, q] of Object.entries(nmiHistoryQueries(scope))) {
      expect(q.filters?.psp, name).toEqual(["psp_1"])
      expect(q.filters?.owner, name).toBeUndefined()
      expect(q.range, name).toEqual({ last: "25m" })
    }
  })
})

describe("shaping", () => {
  it("finds the month OpenRails began recording", () => {
    expect(recordingSince(result("recorded"))).toBe(OCT)
    expect(recordingSince({ ...result("recorded"), rows: [] })).toBeNull()
  })

  it("lays out refusal rates by month and kind, newest first", () => {
    const months = nmiHistoryMonths(result("months"), OCT)
    expect(months.map((m) => [m.month, m.recorded])).toEqual([
      [OCT, true],
      ["2022-08-01T00:00:00Z", false],
      ["2022-07-01T00:00:00Z", false],
    ])
    expect(months[0].kinds).toEqual({
      verification: { authorizations: 1, refusalRate: 0 },
    })
    expect(months[1].kinds.scheduled_rebill).toEqual({
      authorizations: 2,
      refusalRate: 0.5,
    })
    expect(months[2].kinds.one_off_sale.refusalRate).toBeCloseTo(1 / 3)
    expect(nmiHistoryMonths(result("months"), null).some((m) => m.recorded))
      .toBe(false)
  })

  it("ranks refusal reasons with their share of every refusal", () => {
    expect(nmiRefusals(result("reasons"))).toEqual([
      {
        kind: "one_off_sale",
        category: "issuer_soft",
        reason: "insufficient_funds",
        count: 1,
        share: 0.5,
      },
      {
        kind: "scheduled_rebill",
        category: "issuer_soft",
        reason: "do_not_honor",
        count: 1,
        share: 0.5,
      },
    ])
  })
})

describe("page", () => {
  const nmi: Partial<PSP> = {
    id: "psp-1",
    key: "mobius",
    rail: "nmi",
    account_id: "acct-1",
  }
  const page = (providers: Partial<PSP>[]) => {
    const queries = client()
    queries.setQueryData(adminQueries.psps().queryKey, {
      data: providers as PSP[],
      next_cursor: null,
    })
    const built = nmiHistoryQueries(all) as Record<string, MetricsQuery>
    for (const [name, { result }] of Object.entries(captured))
      queries.setQueryData(
        adminQueries.widgetMetrics(built[name]).queryKey,
        result
      )
    return render(<PaymentHealthPage />, queries)
  }

  it("shows NMI's history apart from OpenRails' own attempts", () => {
    const html = page([nmi])
    expect(html).toContain("NMI history (before OpenRails recorded attempts)")
    expect(html).toContain(
      `OpenRails recorded its own attempts from ${formatBucket(OCT, "month")}`
    )
    expect(html).toContain("OpenRails starts")
    expect(html).toContain("33.33%")
    expect(html).toContain("50%")
    expect(html).toContain("do_not_honor")
  })

  it("is absent without an NMI PSP", () => {
    const stripe: Partial<PSP> = {
      id: "psp-2",
      rail: "stripe",
      account_id: "acct_2",
    }
    expect(page([stripe])).not.toContain("NMI history")
  })
})
