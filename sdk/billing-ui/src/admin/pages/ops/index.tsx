import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
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
import type { Finding } from "#orck/client/generated/wire"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
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
  low: "bg-muted text-muted-foreground",
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
      {metrics && gauges && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
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
                  <TableCell className="text-xs">{f.finding_type}</TableCell>
                  <TableCell className="max-w-52 truncate text-xs">
                    {f.subject_key}
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={f.status} />
                  </TableCell>
                  <TableCell>
                    {f.recommendation?.action ?? f.recommended_action ?? "—"}
                  </TableCell>
                  <TableCell>{formatDate(f.last_seen_at)}</TableCell>
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
