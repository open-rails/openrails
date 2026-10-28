import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useSearchParams } from "#orck/admin/router"

import { StatusBadge } from "#orck/admin/components/status-badge"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import { Textarea } from "#orck/admin/components/ui/textarea"
import { ApiError } from "#orck/admin/lib/api/client"
import type { ProposalFilters } from "#orck/admin/lib/api/import-endpoints"
import {
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import { shortId, timeAgo } from "#orck/admin/lib/format"
import type { Finding, ProposalResult } from "#orck/client/generated/wire"
import { chunks } from "#orck/admin/pages/imports/mapping-csv"
import { pspKey, usePSPs } from "#orck/admin/pages/imports/shared"
import {
  EFFECTS,
  effective,
  groupProposals,
  kindLabel,
  type ProposalGroup,
} from "./grouping"

type Action = "approve" | "dismiss"

// A run is a listed set sent 100 ids a call, in order. Each batch's
// Idempotency-Key is made when the set is confirmed, so a retry of a batch
// replays it instead of approving twice.
interface Run {
  action: Action
  notes: string
  batches: { ids: string[]; key: string }[]
  next: number
}

export function ProposalsPage() {
  return useMerchantConfig() ? <Proposals /> : null
}

function Proposals() {
  const { text } = useAdminUI()
  const { importQueries, importMutations } = useAdminServices()
  const queryClient = useQueryClient()
  const canUpdate = useMerchantConfigUpdates()
  const psps = usePSPs()
  const [params] = useSearchParams()
  const filters: ProposalFilters = params.get("ids")
    ? { ids: params.get("ids")! }
    : {
        psp_id: params.get("psp_id") || undefined,
        pull_id: params.get("pull_id") || undefined,
        kind: params.get("kind") || undefined,
      }
  const { data, isPending, error, refetch, isFetching } = useQuery(
    importQueries.proposals(filters)
  )
  const decide = useMutation(importMutations.decide(queryClient))
  const [selected, setSelected] = React.useState<Set<string>>(new Set())
  const [outcomes, setOutcomes] = React.useState(
    new Map<string, ProposalResult>()
  )
  const [confirming, setConfirming] = React.useState<Action>()
  const [failure, setFailure] = React.useState<{ run: Run; message: string }>()
  const findings = React.useMemo(() => data ?? [], [data])
  const groups = groupProposals(findings)
  const open = (f: Finding) => !f.resolved_at && !outcomes.has(f.id)
  const chosen = findings.filter((f) => selected.has(f.id))

  const execute = async (run: Run) => {
    setFailure(undefined)
    for (let i = run.next; i < run.batches.length; i++) {
      const batch = run.batches[i]
      try {
        const result = await decide.mutateAsync({
          action: run.action,
          ids: batch.ids,
          notes: run.notes,
          key: batch.key,
        })
        setOutcomes((prev) => {
          const next = new Map(prev)
          for (const r of result.data) next.set(r.id, r)
          return next
        })
        run.next = i + 1
      } catch (err) {
        const message =
          err instanceof ApiError && err.code === "person_required"
            ? text(
                "Approvals and dismissals need a signed-in person: an API key or service token cannot decide them."
              )
            : err instanceof Error
              ? err.message
              : String(err)
        setFailure({ run, message })
        return
      }
    }
    setSelected(new Set())
  }

  const toggle = (ids: string[], on: boolean) =>
    setSelected((prev) => {
      const next = new Set(prev)
      for (const id of ids) {
        if (on) next.add(id)
        else next.delete(id)
      }
      return next
    })

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">
            <AdminMessage id="Proposals" />
          </h1>
          <p className="text-sm text-muted-foreground">
            <AdminMessage id="Differences a PSP pull found in what OpenRails holds. Nothing changes until a person approves it." />
          </p>
          {(filters.pull_id || filters.ids) && (
            <p className="text-sm">
              {filters.pull_id && (
                <Link
                  className="underline"
                  to={`/ops/imports/${filters.pull_id}`}
                >
                  {text("From import ")}
                  {shortId(filters.pull_id, 14)}
                </Link>
              )}{" "}
              <Link className="underline" to="/ops/proposals">
                <AdminMessage id="All open proposals" />
              </Link>
            </p>
          )}
        </div>
        <div className="flex gap-2">
          <Button
            variant="outline"
            disabled={isFetching || decide.isPending}
            onClick={() => {
              setOutcomes(new Map())
              setSelected(new Set())
              void refetch()
            }}
          >
            <AdminMessage id="Reload" />
          </Button>
          {canUpdate && (
            <>
              <Button
                variant="outline"
                disabled={!chosen.length || decide.isPending}
                onClick={() => setConfirming("dismiss")}
              >
                {text("Dismiss ")}
                {chosen.length}
              </Button>
              <Button
                disabled={!chosen.length || decide.isPending}
                onClick={() => setConfirming("approve")}
              >
                {text("Approve ")}
                {chosen.length}
              </Button>
            </>
          )}
        </div>
      </div>
      {failure && (
        <div
          role="alert"
          className="flex items-center gap-3 rounded-md border border-destructive/40 p-3 text-sm"
        >
          <span className="text-destructive">{failure.message}</span>
          <Button
            size="sm"
            variant="outline"
            disabled={decide.isPending}
            onClick={() => void execute(failure.run)}
          >
            <AdminMessage id="Retry" />
          </Button>
        </div>
      )}
      {error ? (
        <p role="alert" className="text-destructive">
          {error.message}
        </p>
      ) : isPending ? (
        <p role="status">
          <AdminMessage id="Loading…" />
        </p>
      ) : !groups.length ? (
        <p className="text-sm text-muted-foreground">
          <AdminMessage id="No proposals are open." />
        </p>
      ) : (
        groups.map((group) => (
          <Group
            key={group.key}
            group={group}
            psp={group.psp ? pspKey(psps.byId, group.psp) : ""}
            selectable={canUpdate}
            open={open}
            selected={selected}
            outcomes={outcomes}
            onToggle={toggle}
          />
        ))
      )}
      {confirming && (
        <DecideDialog
          action={confirming}
          findings={chosen}
          onClose={() => setConfirming(undefined)}
          onConfirm={(notes) => {
            setConfirming(undefined)
            void execute({
              action: confirming,
              notes,
              next: 0,
              batches: chunks(chosen.map((f) => f.id)).map((ids) => ({
                ids,
                key: crypto.randomUUID(),
              })),
            })
          }}
        />
      )}
    </div>
  )
}

function Group({
  group,
  psp,
  selectable,
  open,
  selected,
  outcomes,
  onToggle,
}: {
  group: ProposalGroup
  psp: string
  selectable: boolean
  open: (f: Finding) => boolean
  selected: Set<string>
  outcomes: Map<string, ProposalResult>
  onToggle: (ids: string[], on: boolean) => void
}) {
  const { text } = useAdminUI()
  const openIds = group.items.filter(open).map((f) => f.id)
  const all = openIds.length > 0 && openIds.every((id) => selected.has(id))
  return (
    <section className="rounded-md border" data-group={group.kind}>
      <div className="flex flex-wrap items-center justify-between gap-2 border-b p-3">
        <h2 className="font-medium">
          {psp && `${psp} · `}
          {group.items.length} {text(kindLabel(group.kind))}
        </h2>
        {selectable && openIds.length > 0 && (
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={all}
              onChange={(e) => onToggle(openIds, e.target.checked)}
            />
            {text("Select all ")}
            {openIds.length}
          </label>
        )}
      </div>
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              {[
                "",
                "Customer",
                "Subject",
                "Change",
                "Effects",
                "Evidence",
                "",
              ].map((h, i) => (
                <TableHead key={i} className="text-muted-foreground">
                  {h && text(h)}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {group.items.map((f) => (
              <Row
                key={f.id}
                finding={f}
                selectable={selectable && open(f)}
                selected={selected.has(f.id)}
                outcome={outcomes.get(f.id)}
                onToggle={(on) => onToggle([f.id], on)}
              />
            ))}
          </TableBody>
        </Table>
      </div>
    </section>
  )
}

const compact = (v: Record<string, unknown> | null) =>
  v
    ? Object.entries(v)
        .map(
          ([k, x]) =>
            `${k}: ${typeof x === "object" ? JSON.stringify(x) : String(x)}`
        )
        .join(", ")
    : "—"

function Row({
  finding: f,
  selectable,
  selected,
  outcome,
  onToggle,
}: {
  finding: Finding
  selectable: boolean
  selected: boolean
  outcome?: ProposalResult
  onToggle: (on: boolean) => void
}) {
  const { text } = useAdminUI()
  const change = f.change
  return (
    <TableRow data-proposal={f.id}>
      <TableCell>
        {selectable && (
          <input
            type="checkbox"
            aria-label={`${text("Select ")}${f.id}`}
            checked={selected}
            onChange={(e) => onToggle(e.target.checked)}
          />
        )}
      </TableCell>
      <TableCell className="font-mono text-xs">
        {change?.customer_id ? (
          <Link className="underline" to={`/customers/${change.customer_id}`}>
            {shortId(change.customer_id, 13)}
          </Link>
        ) : (
          "—"
        )}
      </TableCell>
      <TableCell className="text-xs">
        {change
          ? `${change.subject.type} ${shortId(change.subject.id, 16)}`
          : f.subject_key}
      </TableCell>
      <TableCell className="max-w-80 text-xs">
        {change
          ? `${compact(change.from)} → ${compact(change.to)}`
          : (f.recommended_action ?? text("review only"))}
      </TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {EFFECTS.filter((e) => change?.effects[e]).map((e) => (
            <Badge
              key={e}
              variant="secondary"
              className="bg-held-surface text-held"
            >
              {text(e.replace("_", " "))}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell className="text-xs">
        {change && (
          <details>
            <summary>
              {change.evidence.read} · {timeAgo(change.observed_at)}
            </summary>
            <pre className="max-w-96 overflow-auto">
              {JSON.stringify(change.evidence.provider, null, 2)}
            </pre>
          </details>
        )}
      </TableCell>
      <TableCell className="text-right" data-testid="outcome">
        {outcome ? (
          <span className="flex flex-col items-end gap-0.5">
            <StatusBadge status={outcome.outcome} />
            {outcome.error && (
              <span className="text-xs text-muted-foreground">
                {outcome.error.code}
              </span>
            )}
          </span>
        ) : f.resolved_at ? (
          <StatusBadge status={f.resolution ?? f.status} />
        ) : null}
      </TableCell>
    </TableRow>
  )
}

// DecideDialog confirms a listed set: dismissing needs notes; approving a
// set that ends access, moves money or writes to a provider needs its count
// typed.
function DecideDialog({
  action,
  findings,
  onClose,
  onConfirm,
}: {
  action: Action
  findings: Finding[]
  onClose: () => void
  onConfirm: (notes: string) => void
}) {
  const { text, id: adminId } = useAdminUI()
  const [notes, setNotes] = React.useState("")
  const [typed, setTyped] = React.useState("")
  const weighty = action === "approve" ? effective(findings) : 0
  const count = String(findings.length)
  const ready =
    (action === "approve" || notes.trim() !== "") &&
    (!weighty || typed === count)
  const title = action === "approve" ? text("Approve") : text("Dismiss")
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            {title} {count}
          </DialogTitle>
          <DialogDescription>
            {action === "approve"
              ? text(
                  "Each approved change applies on its own, if what it was based on has not moved."
                )
              : text("Dismissed proposals close without changing anything.")}
            {findings.length > 100 &&
              ` ${text("Sent in ")}${Math.ceil(findings.length / 100)}${text(" requests.")}`}
          </DialogDescription>
        </DialogHeader>
        {weighty > 0 && (
          <p className="text-held text-sm">
            {weighty}
            {text(" of them end access, move money or write to a provider.")}
          </p>
        )}
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("decide-notes")}>
            {action === "dismiss"
              ? text("Notes (required)")
              : text("Notes (optional)")}
          </Label>
          <Textarea
            id={adminId("decide-notes")}
            value={notes}
            onChange={(e) => setNotes(e.target.value)}
          />
        </div>
        {weighty > 0 && (
          <div className="grid gap-1.5">
            <Label htmlFor={adminId("decide-count")}>
              {text("Type ")}
              <span className="font-semibold">{count}</span>
              {text(" to confirm")}
            </Label>
            <Input
              id={adminId("decide-count")}
              value={typed}
              autoComplete="off"
              onChange={(e) => setTyped(e.target.value)}
            />
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            <AdminMessage id="Cancel" />
          </Button>
          <Button
            variant={action === "approve" ? "default" : "destructive"}
            disabled={!ready}
            onClick={() => onConfirm(notes.trim())}
          >
            {title}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
