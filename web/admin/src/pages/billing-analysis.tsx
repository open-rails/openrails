import * as React from "react"
import { useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"

import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import type {
  BillingAnalysisCharge,
  BillingAnalysisDay,
  BillingAnalysisFailure,
  BillingAnalysisUnbilledMember,
} from "@/lib/api/billing-analysis"
import { formatDate } from "@/lib/format"
import { adminQueries } from "@/lib/queries"

function isoDate(date: Date): string {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, "0")
  const day = String(date.getDate()).padStart(2, "0")
  return `${year}-${month}-${day}`
}

function defaultFrom(): string {
  const date = new Date()
  date.setDate(date.getDate() - 29)
  return isoDate(date)
}

function today(): string {
  return isoDate(new Date())
}

function dayLabel(value: string): string {
  const parsed = new Date(`${value}T00:00:00`)
  return Number.isNaN(parsed.getTime())
    ? value
    : parsed.toLocaleDateString(undefined, { month: "short", day: "numeric" })
}

function eventTotal(day: BillingAnalysisDay): number {
  return (
    day.signups +
    day.rebills +
    day.settled_other +
    day.failed_signups +
    day.failed_rebills +
    day.failed_other
  )
}

function amount(value: string | number | undefined, currency?: string): string {
  if (value == null || value === "") return "—"
  const cents = Number(value)
  if (!Number.isFinite(cents)) return String(value)
  return new Intl.NumberFormat(undefined, {
    style: "currency",
    currency: currency || "USD",
  }).format(cents / 100)
}

function subject(row: {
  customer_email?: string
  customer_ref?: string
  email?: string
  order_ref?: string
  subscription_ref?: string
}): string {
  return (
    row.customer_email ||
    row.email ||
    row.customer_ref ||
    row.order_ref ||
    row.subscription_ref ||
    "Unknown member"
  )
}

function memberDate(value?: string): string {
  if (!value) return "—"
  // Date-only values should not shift a day when displayed in a local timezone.
  return value.length === 10 ? dayLabel(value) : formatDate(value)
}

function csvEscape(value: unknown): string {
  const text = value == null ? "" : String(value)
  return /[",\n]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text
}

function downloadUnbilled(rows: BillingAnalysisUnbilledMember[]) {
  const header = [
    "id",
    "customer_ref",
    "subscription_id",
    "email",
    "provider",
    "status",
    "unbilled_since",
    "last_failed_at",
    "failure_count",
    "failure_code",
    "failure_reason",
  ]
  const csv = [
    header.join(","),
    ...rows.map((row) =>
      [
        row.id,
        row.customer_ref,
        row.subscription_id,
        row.email,
        row.provider,
        row.status,
        row.unbilled_since,
        row.last_failed_at,
        row.failure_count,
        row.failure_code,
        row.failure_reason,
      ]
        .map(csvEscape)
        .join(",")
    ),
  ].join("\n")
  const url = URL.createObjectURL(
    new Blob([csv], { type: "text/csv;charset=utf-8" })
  )
  const anchor = document.createElement("a")
  anchor.href = url
  anchor.download = `unbilled-members-${today()}.csv`
  anchor.click()
  URL.revokeObjectURL(url)
}

function rawEvidence(value: unknown): React.ReactNode {
  if (value == null) return null
  let text: string
  try {
    text = JSON.stringify(value, null, 2)
  } catch {
    text = String(value)
  }
  return (
    <details className="max-w-72">
      <summary className="cursor-pointer text-xs text-muted-foreground">
        Raw payload
      </summary>
      <pre className="mt-2 max-h-48 overflow-auto rounded bg-muted p-2 text-[10px] break-all whitespace-pre-wrap">
        {text}
      </pre>
    </details>
  )
}

function ChargeTable({ rows }: { rows: BillingAnalysisCharge[] }) {
  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Kind</TableHead>
            <TableHead>Member</TableHead>
            <TableHead>Provider/source</TableHead>
            <TableHead>Reference</TableHead>
            <TableHead>Amount</TableHead>
            <TableHead>Raw</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={6}
                className="h-20 text-center text-muted-foreground"
              >
                No successful charges for this day.
              </TableCell>
            </TableRow>
          ) : (
            rows.map((row) => (
              <TableRow key={row.event_key}>
                <TableCell className="font-medium">{row.kind}</TableCell>
                <TableCell>
                  <div className="grid gap-0.5">
                    <span>{subject(row)}</span>
                    {row.customer_ref && row.customer_email && (
                      <span className="text-xs text-muted-foreground">
                        {row.customer_ref}
                      </span>
                    )}
                  </div>
                </TableCell>
                <TableCell>
                  <div className="grid gap-0.5">
                    <span>{row.provider || "—"}</span>
                    <span className="text-xs text-muted-foreground">
                      {row.source || "unknown source"}
                    </span>
                  </div>
                </TableCell>
                <TableCell className="max-w-48">
                  <span
                    className="block truncate text-xs"
                    title={row.transaction_id || row.subscription_ref}
                  >
                    {row.transaction_id ||
                      row.subscription_ref ||
                      row.order_ref ||
                      "—"}
                  </span>
                </TableCell>
                <TableCell className="whitespace-nowrap tabular-nums">
                  {amount(row.amount_cents, row.currency)}
                </TableCell>
                <TableCell>{rawEvidence(row.raw)}</TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </div>
  )
}

function FailureTable({ rows }: { rows: BillingAnalysisFailure[] }) {
  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Kind</TableHead>
            <TableHead>Member</TableHead>
            <TableHead>Provider/source</TableHead>
            <TableHead>Reference</TableHead>
            <TableHead>Reason</TableHead>
            <TableHead>Raw</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={6}
                className="h-20 text-center text-muted-foreground"
              >
                No failed charge attempts for this day.
              </TableCell>
            </TableRow>
          ) : (
            rows.map((row) => (
              <TableRow key={row.event_key}>
                <TableCell className="font-medium">{row.kind}</TableCell>
                <TableCell>
                  <div className="grid gap-0.5">
                    <span>{subject(row)}</span>
                    {row.customer_ref && row.customer_email && (
                      <span className="text-xs text-muted-foreground">
                        {row.customer_ref}
                      </span>
                    )}
                  </div>
                </TableCell>
                <TableCell>
                  <div className="grid gap-0.5">
                    <span>{row.provider || "—"}</span>
                    <span className="text-xs text-muted-foreground">
                      {row.source || "unknown source"}
                    </span>
                  </div>
                </TableCell>
                <TableCell className="max-w-48">
                  <span
                    className="block truncate text-xs"
                    title={row.transaction_id || row.subscription_ref}
                  >
                    {row.transaction_id ||
                      row.subscription_ref ||
                      row.order_ref ||
                      "—"}
                  </span>
                </TableCell>
                <TableCell className="max-w-64">
                  <div
                    className="truncate text-xs"
                    title={row.decline_reason || row.decline_code}
                  >
                    {row.decline_reason ||
                      row.decline_code ||
                      "Unknown failure"}
                    <span className="ml-1 text-muted-foreground">
                      ({row.failure_count})
                    </span>
                  </div>
                </TableCell>
                <TableCell>{rawEvidence(row.raw)}</TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </div>
  )
}

export function BillingAnalysisPage() {
  const [params, setParams] = useSearchParams()
  const from = params.get("from") || defaultFrom()
  const to = params.get("to") || today()
  const provider = params.get("provider") || ""
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
  const [selectedDay, setSelectedDay] = React.useState(to)

  const filters = {
    from,
    to,
    timezone,
    ...(provider ? { provider } : {}),
  }
  const { data, isPending, isError, error } = useQuery(
    adminQueries.billingAnalysis(filters)
  )

  const setFilter = (key: string, value: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
    if (key === "to") setSelectedDay(value)
  }

  const daily = data?.daily ?? []
  const maxEvents = Math.max(1, ...daily.map(eventTotal))
  const selectedSummary = daily.find((day) => day.date === selectedDay)
  const members = selectedSummary?.unbilled ?? []
  const signupTotal = daily.reduce((sum, day) => sum + day.signups, 0)
  const rebillTotal = daily.reduce((sum, day) => sum + day.rebills, 0)
  const failedTotal = daily.reduce((sum, day) => sum + day.failed_rebills, 0)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">
            Billing analysis
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Daily provider evidence and members with an unpaid recurring charge.
          </p>
        </div>
        <Button
          size="sm"
          variant="outline"
          disabled={members.length === 0}
          onClick={() => downloadUnbilled(members)}
        >
          Export unbilled CSV
        </Button>
      </div>

      <div className="flex flex-wrap items-end gap-3 rounded-xl border bg-card p-4">
        <label className="grid gap-1 text-xs font-medium text-muted-foreground">
          From
          <Input
            type="date"
            value={from}
            max={to}
            onChange={(event) => setFilter("from", event.target.value)}
            className="w-40 text-foreground"
          />
        </label>
        <label className="grid gap-1 text-xs font-medium text-muted-foreground">
          To
          <Input
            type="date"
            value={to}
            min={from}
            onChange={(event) => setFilter("to", event.target.value)}
            className="w-40 text-foreground"
          />
        </label>
        <label className="grid gap-1 text-xs font-medium text-muted-foreground">
          Provider
          <Select
            value={provider || "all"}
            onValueChange={(value) =>
              setFilter("provider", !value || value === "all" ? "" : value)
            }
          >
            <SelectTrigger className="w-40 text-foreground">
              <SelectValue placeholder="All providers" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All providers</SelectItem>
              {(data?.providers ?? []).map((name) => (
                <SelectItem key={name} value={name}>
                  {name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </label>
        <span className="pb-1 text-xs text-muted-foreground">
          {data?.timezone ? `Times shown in ${data.timezone}` : ""}
        </span>
      </div>

      {isError ? (
        <Card>
          <CardContent className="py-10 text-center text-sm text-destructive">
            Could not load billing analysis: {error.message}
          </CardContent>
        </Card>
      ) : (
        <>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <SummaryCard label="New signups" value={signupTotal} />
            <SummaryCard label="Successful rebills" value={rebillTotal} />
            <SummaryCard
              label="Failed rebills"
              value={failedTotal}
              tone="failed"
            />
            <SummaryCard
              label="Delinquent accounts"
              value={
                selectedSummary?.delinquent_users ??
                data?.delinquent?.length ??
                0
              }
              tone="held"
            />
          </div>

          <Card>
            <CardHeader>
              <CardTitle>Daily activity</CardTitle>
              <CardDescription>
                Select a day to inspect the unbilled cases that were already
                open.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Date</TableHead>
                      <TableHead className="text-right">Signups</TableHead>
                      <TableHead className="text-right">Rebills</TableHead>
                      <TableHead className="text-right">
                        Other success
                      </TableHead>
                      <TableHead className="text-right">
                        Failed signups
                      </TableHead>
                      <TableHead className="text-right">
                        Failed rebills
                      </TableHead>
                      <TableHead className="text-right">
                        Other failures
                      </TableHead>
                      <TableHead className="text-right">Delinquent</TableHead>
                      <TableHead className="text-right">
                        Open unbilled
                      </TableHead>
                      <TableHead className="min-w-36">Activity</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {isPending ? (
                      <TableRow>
                        <TableCell
                          colSpan={10}
                          className="h-28 text-center text-muted-foreground"
                        >
                          Loading…
                        </TableCell>
                      </TableRow>
                    ) : daily.length === 0 ? (
                      <TableRow>
                        <TableCell
                          colSpan={10}
                          className="h-28 text-center text-muted-foreground"
                        >
                          No billing activity in this range.
                        </TableCell>
                      </TableRow>
                    ) : (
                      daily.map((day) => {
                        const total = eventTotal(day)
                        return (
                          <TableRow
                            key={day.date}
                            data-selected={
                              day.date === selectedDay ? "true" : undefined
                            }
                            className="cursor-pointer"
                            onClick={() => setSelectedDay(day.date)}
                          >
                            <TableCell className="font-medium">
                              {dayLabel(day.date)}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.signups}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.rebills}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.settled_other}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.failed_signups}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.failed_rebills}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.failed_other}
                            </TableCell>
                            <TableCell className="text-right tabular-nums">
                              {day.delinquent_users}
                            </TableCell>
                            <TableCell className="text-right font-medium tabular-nums">
                              {day.open_unbilled}
                            </TableCell>
                            <TableCell>
                              <div
                                className="h-2 overflow-hidden rounded-full bg-muted"
                                aria-label={`${total} events`}
                              >
                                <div
                                  className="h-full rounded-full bg-primary"
                                  style={{
                                    width: `${(total / maxEvents) * 100}%`,
                                  }}
                                />
                              </div>
                            </TableCell>
                          </TableRow>
                        )
                      })
                    )}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
          </Card>

          <div className="grid gap-4 xl:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>Successful charges</CardTitle>
                <CardDescription>
                  {selectedDay
                    ? `Settled provider events on ${dayLabel(selectedDay)}`
                    : "Settled provider events"}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <ChargeTable rows={selectedSummary?.charges ?? []} />
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle>Failed charge attempts</CardTitle>
                <CardDescription>
                  Failed signup, rebill, and other attempts remain visible with
                  their provider reason and raw payload.
                </CardDescription>
              </CardHeader>
              <CardContent>
                <FailureTable rows={selectedSummary?.failures ?? []} />
              </CardContent>
            </Card>
          </div>

          <Card>
            <CardHeader className="flex-row items-center justify-between gap-3">
              <div>
                <CardTitle>Unbilled members</CardTitle>
                <CardDescription>
                  {selectedDay
                    ? `Cases open on ${dayLabel(selectedDay)}`
                    : "Open cases"}
                </CardDescription>
              </div>
              <span className="text-sm text-muted-foreground tabular-nums">
                {members.length} member{members.length === 1 ? "" : "s"}
              </span>
            </CardHeader>
            <CardContent>
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Member</TableHead>
                      <TableHead>Provider</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Unbilled since</TableHead>
                      <TableHead>Last failure</TableHead>
                      <TableHead className="text-right">Failures</TableHead>
                      <TableHead>Reason</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {members.length === 0 ? (
                      <TableRow>
                        <TableCell
                          colSpan={7}
                          className="h-28 text-center text-muted-foreground"
                        >
                          No unbilled members for this selection.
                        </TableCell>
                      </TableRow>
                    ) : (
                      members.map((member) => (
                        <TableRow key={member.id}>
                          <TableCell>
                            <div className="grid gap-0.5">
                              <span className="font-medium">
                                {subject(member)}
                              </span>
                              {member.customer_ref && member.email && (
                                <span className="text-xs text-muted-foreground">
                                  {member.customer_ref}
                                </span>
                              )}
                              {member.subscription_id && (
                                <span className="text-xs text-muted-foreground">
                                  subscription: {member.subscription_id}
                                </span>
                              )}
                            </div>
                          </TableCell>
                          <TableCell>{member.provider || "—"}</TableCell>
                          <TableCell>
                            <StatusBadge status={member.status} />
                          </TableCell>
                          <TableCell className="tabular-nums">
                            {memberDate(member.unbilled_since)}
                          </TableCell>
                          <TableCell className="tabular-nums">
                            {memberDate(member.last_failed_at)}
                          </TableCell>
                          <TableCell className="text-right tabular-nums">
                            {member.failure_count}
                          </TableCell>
                          <TableCell>
                            <div
                              className="max-w-56 truncate text-xs text-muted-foreground"
                              title={
                                member.failure_reason || member.failure_code
                              }
                            >
                              {member.failure_reason ||
                                member.failure_code ||
                                "—"}
                            </div>
                          </TableCell>
                        </TableRow>
                      ))
                    )}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}

function SummaryCard({
  label,
  value,
  tone,
}: {
  label: string
  value: number
  tone?: "failed" | "held"
}) {
  return (
    <Card>
      <CardHeader className="pb-1">
        <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
          {label}
        </CardTitle>
      </CardHeader>
      <CardContent
        className={`text-2xl font-semibold tabular-nums ${tone === "failed" ? "text-failed" : tone === "held" ? "text-held" : ""}`}
      >
        {value.toLocaleString()}
      </CardContent>
    </Card>
  )
}
