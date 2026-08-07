import * as React from "react"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Label } from "@/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import type { Finding } from "@/lib/api/types"
import { formatDate } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"

export function OpsPage() {
  return (
    <Tabs defaultValue="findings" className="flex flex-col gap-4">
      <TabsList>
        <TabsTrigger value="findings">Findings</TabsTrigger>
        <TabsTrigger value="repair-alerts">Repair alerts</TabsTrigger>
        <TabsTrigger value="worker-health">Worker health</TabsTrigger>
      </TabsList>
      <TabsContent value="findings">
        <FindingsTab />
      </TabsContent>
      <TabsContent value="repair-alerts">
        <RepairAlertsTab />
      </TabsContent>
      <TabsContent value="worker-health">
        <WorkerHealthTab />
      </TabsContent>
    </Tabs>
  )
}

const severityTone: Record<string, string> = {
  critical: "bg-failed-surface text-failed",
  high: "bg-held-surface text-held",
  medium: "bg-held-surface text-held",
  low: "bg-muted text-muted-foreground",
}

function FindingsTab() {
  const { data, isPending: loading } = useQuery(adminQueries.findings())
  const [resolving, setResolving] = React.useState<Finding | null>(null)

  const gauges = data?.gauges
  return (
    <div className="flex flex-col gap-4">
      {gauges && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Gauge label="Open findings" value={gauges.total_open} />
          <Gauge
            label="Orphaned members"
            value={gauges.orphaned_members}
            alert
          />
          <Gauge label="Freeloaders" value={gauges.freeloaders} alert />
          <Gauge
            label="Duplicate coverage"
            value={gauges.duplicate_coverage}
            alert
          />
        </div>
      )}
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : !data?.items?.length ? (
        <p className="text-sm text-muted-foreground">
          No open findings — queue is clear.
        </p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Severity</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Subject</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Recommendation</TableHead>
                <TableHead>Last seen</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.items.map((f) => (
                <TableRow key={f.id}>
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
                    {!f.resolved_at && (
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setResolving(f)}
                      >
                        Resolve
                      </Button>
                    )}
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
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>Resolve finding</DialogTitle>
          <DialogDescription>
            {finding.finding_type} · {finding.subject_key}
          </DialogDescription>
        </DialogHeader>
        {finding.recommendation ? (
          <div className="rounded-md bg-muted p-3 text-sm">
            <p className="font-medium">
              Recommended: {finding.recommendation.action}
            </p>
            {finding.recommendation.params && (
              <pre className="mt-1 overflow-auto text-xs">
                {JSON.stringify(finding.recommendation.params, null, 2)}
              </pre>
            )}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            No mechanical fix is attached — this finding can only be ignored
            (with notes).
          </p>
        )}
        <form.Field name="notes">
          {(field) => (
            <div className="grid gap-1.5">
              <Label htmlFor="f-notes">
                Notes {canApprove ? "(required to ignore)" : "(required)"}
              </Label>
              <Textarea
                id="f-notes"
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
                Ignore
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

function RepairAlertsTab() {
  const { data, isPending: loading } = useQuery(adminQueries.repairAlerts())
  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!data?.data?.length)
    return <p className="text-sm text-muted-foreground">No repair alerts.</p>
  return (
    <div className="overflow-x-auto rounded-md border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Event</TableHead>
            <TableHead>Customer</TableHead>
            <TableHead>Seen</TableHead>
            <TableHead>Created</TableHead>
            <TableHead>Data</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.data.map((a) => (
            <TableRow key={a.id}>
              <TableCell className="text-xs">{a.event_type}</TableCell>
              <TableCell className="text-xs">{a.customer_id ?? "—"}</TableCell>
              <TableCell>{a.seen ? "yes" : "no"}</TableCell>
              <TableCell>{formatDate(a.created_at)}</TableCell>
              <TableCell className="max-w-72 truncate text-xs">
                {a.data ? JSON.stringify(a.data) : "—"}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function WorkerHealthTab() {
  const { data, isPending: loading } = useQuery(adminQueries.workerHealth())
  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!data?.length)
    return (
      <p className="text-sm text-muted-foreground">No workers registered.</p>
    )
  return (
    <div className="overflow-x-auto rounded-md border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Worker</TableHead>
            <TableHead>Last success</TableHead>
            <TableHead>Last error</TableHead>
            <TableHead>Failures</TableHead>
            <TableHead>Expected period</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.map((w) => (
            <TableRow key={w.worker_kind}>
              <TableCell className="text-xs">{w.worker_kind}</TableCell>
              <TableCell>{formatDate(w.last_success_at)}</TableCell>
              <TableCell className="max-w-72">
                {w.last_error_at ? (
                  <span title={w.last_error}>
                    {formatDate(w.last_error_at)}
                    {w.last_error ? ` — ${w.last_error.slice(0, 60)}` : ""}
                  </span>
                ) : (
                  "—"
                )}
              </TableCell>
              <TableCell>
                {w.consecutive_failures > 0 ? (
                  <Badge
                    variant="secondary"
                    className="bg-failed-surface text-failed"
                  >
                    {w.consecutive_failures}
                  </Badge>
                ) : (
                  "0"
                )}
              </TableCell>
              <TableCell>
                {w.expected_period_seconds
                  ? `${w.expected_period_seconds}s`
                  : "—"}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
