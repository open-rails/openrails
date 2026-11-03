import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { STAT_TILES_4 } from "#orck/admin/lib/tile-grids"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"

import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useSearchParams } from "#orck/admin/router"

import { StatusBadge } from "#orck/admin/components/status-badge"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
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
import type { DestructiveHold, Finding } from "#orck/client/generated/wire"
import { ApiError } from "#orck/admin/lib/api/client"
import { DIALOG_CONFIRM, DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import { errorText } from "#orck/admin/lib/error-text"
import { formatDate } from "#orck/admin/lib/format"

import {
  useMerchantConfig,
  useMerchantConfigUpdates,
  useMetrics,
} from "#orck/admin/lib/capabilities"

export function OpsPage() {
  return useMerchantConfig() ? <FindingsTab /> : null
}

const severityTone: Record<string, string> = {
  critical: "bg-failed-surface text-failed",
  high: "bg-held-surface text-held",
  medium: "bg-held-surface text-held",
  low: "bg-inactive-surface text-inactive",
}

function FindingsTab() {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const canRead = useMerchantConfig()
  const canUpdate = useMerchantConfigUpdates()
  const metrics = useMetrics()
  const { data, isPending: loading } = useQuery(adminQueries.findings(canRead))
  const { data: gauges } = useQuery({
    ...adminQueries.findingSummary(),
    enabled: metrics,
  })
  const [resolving, setResolving] = React.useState<Finding | null>(null)
  // The bell and alert emails link one finding: ?finding=<id>.
  const [params] = useSearchParams()
  const linked = params.get("finding")

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight">
        <AdminMessage id={"Findings"} />
      </h1>
      <DestructiveHolds canUpdate={canUpdate} />
      {metrics && gauges && (
        <div className={STAT_TILES_4}>
          {/* Each gauge says what is wrong with the MONEY, not what the check
              is called internally: a merchant reading "freeloaders" cannot tell
              whether that costs them anything. */}
          <Gauge label={adminText("Open issues")} value={gauges.total_open} />
          <Gauge
            label={adminText("Paying, no access")}
            value={gauges.orphaned_members}
            alert
          />
          <Gauge
            label={adminText("Access, not paying")}
            value={gauges.freeloaders}
            alert
          />
          <Gauge
            label={adminText("Charged twice")}
            value={gauges.duplicate_coverage}
            alert
          />
        </div>
      )}
      {loading ? (
        <p className="text-sm text-muted-foreground">
          <AdminMessage id={"Loading…"} />
        </p>
      ) : !data?.data.length ? (
        <p className="text-sm text-muted-foreground">
          <AdminMessage id={"Nothing needs attention right now."} />
        </p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Severity"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Type"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Subject"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Status"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Recommendation"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Last seen"} />
                </TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.data.map((f) => (
                <TableRow
                  key={f.id}
                  className={f.id === linked ? "bg-primary/[0.06]" : undefined}
                >
                  <TableCell>
                    <Badge
                      variant="secondary"
                      className={severityTone[f.severity] ?? ""}
                    >
                      {f.severity}
                    </Badge>
                  </TableCell>
                  <TableCell className="min-w-32 text-xs [overflow-wrap:anywhere] whitespace-normal">
                    {f.finding_type}
                  </TableCell>
                  <TableCell className="max-w-52 truncate text-xs">
                    {f.subject_key}
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={f.status} />
                  </TableCell>
                  <TableCell className="min-w-48 [overflow-wrap:anywhere] whitespace-normal">
                    {f.recommendation?.action ?? f.recommended_action ?? "—"}
                  </TableCell>
                  <TableCell className="min-w-24 whitespace-normal">
                    {formatDate(f.last_seen_at)}
                  </TableCell>
                  <TableCell className="text-right">
                    <FindingAction
                      finding={f}
                      canUpdate={canUpdate}
                      onResolve={() => setResolving(f)}
                    />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
      {resolving && (
        <ResolveFindingDialog
          finding={resolving}
          onClose={() => setResolving(null)}
        />
      )}
    </div>
  )
}

// FindingAction is what a person does with an open finding. A pull's
// proposal is approved or dismissed in the inbox, never resolved here; an
// unmapped provider price is mapped or dismissed among provider prices.
function FindingAction({
  finding: f,
  canUpdate,
  onResolve,
}: {
  finding: Finding
  canUpdate: boolean
  onResolve: () => void
}) {
  if (f.resolved_at) return null
  if (f.finding_type.startsWith("pull."))
    return (
      <Link
        className="text-sm underline"
        to={`/ops/proposals?ids=${encodeURIComponent(f.id)}`}
      >
        <AdminMessage id={"Review"} />
      </Link>
    )
  if (f.finding_type === "catalog.price.unmapped")
    return (
      <Link
        className="text-sm underline"
        to={`/ops/provider-prices?${new URLSearchParams({
          ...(f.psp_id ? { psp: f.psp_id } : {}),
          ref: f.subject_key,
        })}`}
      >
        <AdminMessage id={"Provider prices"} />
      </Link>
    )
  if (!canUpdate) return null
  return (
    <Button variant="outline" size="sm" onClick={onResolve}>
      <AdminMessage id={"Resolve"} />
    </Button>
  )
}

function Gauge({
  label,
  value,
  alert,
}: {
  label: string
  value: number
  alert?: boolean
}) {
  return (
    <Card>
      <CardHeader className="pb-1">
        <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
          {label}
        </CardTitle>
      </CardHeader>
      <CardContent>
        <p
          className={`text-2xl font-semibold ${alert && value > 0 ? "text-failed" : ""}`}
        >
          {value}
        </p>
      </CardContent>
    </Card>
  )
}

function ResolveFindingDialog({
  finding,
  onClose,
}: {
  finding: Finding
  onClose: () => void
}) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { id: adminId } = useAdminUI()

  const queryClient = useQueryClient()
  const resolution = useMutation(adminMutations.resolveFinding(queryClient))
  const form = useForm({ defaultValues: { notes: "" } })
  const canApprove = Boolean(finding.recommendation)

  const resolve = async (outcome: "approve" | "ignore") => {
    try {
      await resolution.mutateAsync({
        id: finding.id,
        outcome,
        notes: form.state.values.notes,
      })
      toast.success(
        outcome === "approve" ? "Recommendation executed" : "Finding ignored"
      )
      onClose()
    } catch (err) {
      toastApiError(err, "Resolve finding")
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id={"Resolve finding"} />
          </DialogTitle>
          <DialogDescription>
            {finding.finding_type} · {finding.subject_key}
          </DialogDescription>
        </DialogHeader>
        {finding.recommendation ? (
          <div className="rounded-md bg-muted p-3 text-sm">
            <p className="font-medium">
              <AdminMessage id={"Recommended: "} />
              {finding.recommendation.action}
            </p>
            {finding.recommendation.params && (
              <pre className="mt-1 overflow-auto text-xs">
                {JSON.stringify(finding.recommendation.params, null, 2)}
              </pre>
            )}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            <AdminMessage
              id={
                "Nothing here can be fixed automatically. You can only note why you are leaving it."
              }
            />
          </p>
        )}
        <form.Field name="notes">
          {(field) => (
            <div className="grid gap-1.5">
              <Label htmlFor={adminId("f-notes")}>
                <AdminMessage id={"Notes "} />
                {canApprove ? "(required to ignore)" : "(required)"}
              </Label>
              <Textarea
                id={adminId("f-notes")}
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={(event) => field.handleChange(event.target.value)}
              />
            </div>
          )}
        </form.Field>
        <DialogFooter>
          <Button
            variant="outline"
            disabled={resolution.isPending}
            onClick={onClose}
          >
            <AdminMessage id={"Cancel"} />
          </Button>
          <form.Subscribe selector={(state) => state.values.notes}>
            {(notes) => (
              <Button
                variant="outline"
                disabled={resolution.isPending || !notes.trim()}
                onClick={() => resolve("ignore")}
              >
                <AdminMessage id={"Ignore"} />
              </Button>
            )}
          </form.Subscribe>
          {canApprove && (
            <Button
              disabled={resolution.isPending}
              onClick={() => resolve("approve")}
            >
              {resolution.isPending ? "Working…" : "Approve recommendation"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// DestructiveHolds lists the open holds the destructive-action limit put on
// refunds, revokes and cancels, and lets a different person clear one.
function DestructiveHolds({ canUpdate }: { canUpdate: boolean }) {
  const { text: adminText } = useAdminUI()
  const { adminQueries } = useAdminServices()
  const { data } = useQuery(adminQueries.destructiveHolds())
  const [clearing, setClearing] = React.useState<DestructiveHold | null>(null)
  const open = data?.data.filter((hold) => !hold.cleared_at) ?? []
  if (!open.length) return null
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">
          <AdminMessage id={"Holds on refunds and cancels"} />
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        {open.map((hold) => (
          <div
            key={hold.id}
            className="flex flex-wrap items-center justify-between gap-3"
          >
            <div className="min-w-0 text-sm">
              <p>
                {adminText(
                  hold.scope === "actor"
                    ? "A staff member reached the limit of"
                    : "Staff reached the limit of"
                )}{" "}
                {hold.ceiling}{" "}
                {adminText(
                  hold.window === "hour"
                    ? "refunds, revokes or cancels in an hour. These are paused until someone clears the hold."
                    : "refunds, revokes or cancels in a day. These are paused until someone clears the hold."
                )}
              </p>
              <p className="text-xs text-muted-foreground">
                <AdminMessage id={"Held since"} /> {formatDate(hold.opened_at)}
              </p>
            </div>
            {canUpdate && (
              <Button variant="outline" onClick={() => setClearing(hold)}>
                <AdminMessage id={"Clear hold"} />
              </Button>
            )}
          </div>
        ))}
      </CardContent>
      {clearing && (
        <ClearHoldDialog hold={clearing} onClose={() => setClearing(null)} />
      )}
    </Card>
  )
}

// clearRefusal says why a clear was refused. The server answers 403 both to
// the person the hold stopped and to one without the unlock permission; only
// its message tells them apart.
function clearRefusal(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 403)
      return /trigger/i.test(error.message)
        ? "A different person must clear this hold."
        : "You don't have permission to clear holds."
    if (error.code === "authorization_unavailable")
      return "Billing couldn't check your permission to clear holds. Try again in a moment."
    if (error.status === 404 || error.code === "resource_conflict")
      return "This hold was already cleared or no longer exists."
  }
  return errorText(error).text
}

function ClearHoldDialog({
  hold,
  onClose,
}: {
  hold: DestructiveHold
  onClose: () => void
}) {
  const { toast, adminMutations } = useAdminServices()
  const { id: adminId, text: adminText } = useAdminUI()
  const queryClient = useQueryClient()
  const clear = useMutation(adminMutations.clearDestructiveHold(queryClient))
  const [reason, setReason] = React.useState("")
  const trimmed = reason.trim()
  return (
    <Dialog open onOpenChange={(o) => !o && !clear.isPending && onClose()}>
      <DialogContent className={DIALOG_CONFIRM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id={"Clear this hold?"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "Refunds, revokes and cancels start working again for the people it held. Someone other than the person it stopped must clear it."
              }
            />
          </DialogDescription>
        </DialogHeader>
        <form
          id={adminId("clear-hold")}
          className="grid gap-1.5"
          onSubmit={async (event) => {
            event.preventDefault()
            if (!trimmed) return
            // Awaited, not a mutate callback: the refreshed list drops the
            // hold and this dialog with it before such a callback would run.
            try {
              await clear.mutateAsync({ id: hold.id, reason: trimmed })
            } catch {
              return // shown from clear.error
            }
            toast.success(adminText("Hold cleared"))
            onClose()
          }}
        >
          <Label htmlFor={adminId("clear-hold-reason")}>
            <AdminMessage id={"Reason (required)"} />
          </Label>
          <Textarea
            id={adminId("clear-hold-reason")}
            value={reason}
            maxLength={2000}
            onChange={(event) => setReason(event.target.value)}
          />
        </form>
        {clear.error && (
          <p role="alert" className="text-sm text-destructive">
            {adminText(clearRefusal(clear.error))}
          </p>
        )}
        <DialogFooter>
          <Button
            variant="outline"
            disabled={clear.isPending}
            onClick={onClose}
          >
            <AdminMessage id={"Cancel"} />
          </Button>
          <Button
            type="submit"
            form={adminId("clear-hold")}
            disabled={!trimmed || clear.isPending}
          >
            {adminText(clear.isPending ? "Clearing…" : "Clear hold")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
