import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link } from "#orck/admin/router"

import { CursorPager } from "#orck/admin/components/cursor-pager"
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
import { ApiError } from "#orck/admin/lib/api/client"
import {
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"
import { useUrlCursor } from "#orck/admin/lib/cursor-pages"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import { formatDate } from "#orck/admin/lib/format"
import type { PSPImport } from "#orck/client/generated/wire"
import { chunks, parseMappingCSV, type MappingRow } from "./mapping-csv"
import { recordCount } from "./progress"
import { ProgressText, ReconcileDialog, pspKey, usePSPs } from "./shared"

const STATES = [
  "linked",
  "unmatched",
  "conflict",
  "blocked",
  "held",
  "imported",
] as const

export function ImportsPage() {
  return useMerchantConfig() ? <Imports /> : null
}

function Imports() {
  const { text } = useAdminUI()
  const { importQueries } = useAdminServices()
  const canUpdate = useMerchantConfigUpdates()
  const psps = usePSPs()
  const paging = useUrlCursor(25)
  const { data, isPending, isFetching } = useQuery(
    importQueries.list({}, paging.size, paging.cursor || undefined)
  )
  const [dialog, setDialog] = React.useState<"reconcile" | "mappings">()
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">
            <AdminMessage id="PSP imports" />
          </h1>
          <p className="text-sm text-muted-foreground">
            <AdminMessage id="Each import pulls one PSP read-only and stages what it read. Link and map what it staged, then apply it." />
          </p>
        </div>
        {canUpdate && (
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => setDialog("mappings")}>
              <AdminMessage id="Upload mappings" />
            </Button>
            <Button onClick={() => setDialog("reconcile")}>
              <AdminMessage id="Reconcile" />
            </Button>
          </div>
        )}
      </div>
      <div className="overflow-x-auto rounded-md border">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              {[
                "Started",
                "PSP",
                "Origin",
                "Status",
                "Progress",
                "Records",
              ].map((h) => (
                <TableHead key={h} className="text-muted-foreground">
                  {text(h)}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {isPending ? (
              <TableRow>
                <TableCell colSpan={6}>
                  <AdminMessage id="Loading…" />
                </TableCell>
              </TableRow>
            ) : !data?.data.length ? (
              <TableRow>
                <TableCell colSpan={6} className="text-muted-foreground">
                  <AdminMessage id="No PSP has been pulled yet." />
                </TableCell>
              </TableRow>
            ) : (
              data.data.map((imp) => (
                <TableRow key={imp.id} data-import={imp.id}>
                  <TableCell>
                    <Link className="underline" to={`/ops/imports/${imp.id}`}>
                      {formatDate(imp.created_at)}
                    </Link>
                  </TableCell>
                  <TableCell>{pspKey(psps.byId, imp.psp)}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">{imp.origin}</Badge>
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={imp.status} />
                  </TableCell>
                  <TableCell className="text-sm">
                    <ProgressText imp={imp} />
                  </TableCell>
                  <TableCell className="text-sm">
                    <CountsText imp={imp} />
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
      <CursorPager
        pages={paging}
        rows={data?.data.length ?? 0}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
      {dialog === "reconcile" && (
        <ReconcileDialog onClose={() => setDialog(undefined)} />
      )}
      {dialog === "mappings" && (
        <MappingsDialog
          imports={data?.data ?? []}
          onClose={() => setDialog(undefined)}
        />
      )}
    </div>
  )
}

function CountsText({ imp }: { imp: PSPImport }) {
  const { text } = useAdminUI()
  const parts = STATES.map((s) => [s, recordCount(imp, s)] as const).filter(
    ([, n]) => n > 0
  )
  if (!parts.length) return <>—</>
  return <>{parts.map(([s, n]) => `${n} ${text(s)}`).join(" · ")}</>
}

type Upload =
  | { step: "review" }
  | { step: "sending"; sent: number; of: number }
  | { step: "done"; set: number; deleted: number }
  | { step: "refused"; set: number; deleted: number; message: string }

// MappingsDialog writes the merchant's mappings from a CSV file: checked in
// the browser, sent 100 rows a request in order, stopping at the first
// refusal. Re-linking an import applies them.
function MappingsDialog({
  imports,
  onClose,
}: {
  imports: PSPImport[]
  onClose: () => void
}) {
  const { text, id: adminId } = useAdminUI()
  const { importMutations, toastApiError, toast } = useAdminServices()
  const queryClient = useQueryClient()
  const send = useMutation(importMutations.setMappings())
  const relink = useMutation(importMutations.link(queryClient))
  const psps = usePSPs()
  const [parsed, setParsed] = React.useState<{
    rows: MappingRow[]
    errors: string[]
  }>()
  const [upload, setUpload] = React.useState<Upload>({ step: "review" })
  const deletes = parsed?.rows.filter((r) => !r.customer).length ?? 0
  const run = async (rows: MappingRow[]) => {
    const parts = chunks(rows)
    let set = 0
    let deleted = 0
    for (const [i, part] of parts.entries()) {
      setUpload({ step: "sending", sent: i, of: parts.length })
      try {
        const result = await send.mutateAsync(
          part.map(({ mapping, key, customer }) => ({ mapping, key, customer }))
        )
        set += result.set
        deleted += result.deleted
      } catch (err) {
        const message = err instanceof ApiError ? err.message : String(err)
        setUpload({
          step: "refused",
          set,
          deleted,
          message: `${text("Lines ")}${part[0].line}–${part.at(-1)!.line}${text(" were refused: ")}${message}`,
        })
        return
      }
    }
    setUpload({ step: "done", set, deleted })
  }
  const relinkable = imports.filter(
    (i) => i.status === "pulled" || i.status === "applied"
  )
  const busy = upload.step === "sending"
  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id="Upload mappings" />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage id="A CSV file with the header mapping,key,customer maps your own identifiers to customers. An empty customer deletes that mapping." />
          </DialogDescription>
        </DialogHeader>
        {upload.step === "review" && (
          <div className="grid gap-1.5">
            <Label htmlFor={adminId("mappings-file")}>
              <AdminMessage id="Mappings file" />
            </Label>
            <Input
              id={adminId("mappings-file")}
              type="file"
              accept=".csv,text/csv"
              onChange={async (e) => {
                const file = e.target.files?.[0]
                setParsed(file ? parseMappingCSV(await file.text()) : undefined)
              }}
            />
          </div>
        )}
        {upload.step === "review" && parsed && (
          <div role="status" className="text-sm">
            {parsed.errors.length ? (
              <ul className="list-disc pl-5 text-destructive">
                {parsed.errors.slice(0, 20).map((e) => (
                  <li key={e}>{e}</li>
                ))}
                {parsed.errors.length > 20 && (
                  <li>
                    {parsed.errors.length - 20}
                    {text(" more")}
                  </li>
                )}
              </ul>
            ) : (
              <p>
                {parsed.rows.length - deletes}
                {text(" to set, ")}
                {deletes}
                {text(" to delete, in ")}
                {chunks(parsed.rows).length}
                {text(" requests.")}
              </p>
            )}
          </div>
        )}
        {upload.step === "sending" && (
          <p role="status" className="text-sm">
            <AdminMessage id="Sending request " />
            {upload.sent + 1}
            <AdminMessage id=" of " />
            {upload.of}…
          </p>
        )}
        {(upload.step === "done" || upload.step === "refused") && (
          <div className="grid gap-2 text-sm">
            {upload.step === "refused" && (
              <p role="alert" className="text-destructive">
                {upload.message}
              </p>
            )}
            <p role="status">
              <AdminMessage id="Mappings set: " />
              {upload.set}
              <AdminMessage id=", deleted: " />
              {upload.deleted}
            </p>
            {relinkable.length > 0 && (
              <div className="grid gap-1">
                <p className="text-muted-foreground">
                  <AdminMessage id="Re-link an import to apply them:" />
                </p>
                {relinkable.map((imp) => (
                  <div key={imp.id} className="flex items-center gap-2">
                    <span>
                      {pspKey(psps.byId, imp.psp)} · {imp.origin} ·{" "}
                      {formatDate(imp.created_at)}
                    </span>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={relink.isPending}
                      onClick={async () => {
                        try {
                          await relink.mutateAsync({ id: imp.id, params: {} })
                          toast.success(text("Re-link requested"))
                        } catch (err) {
                          toastApiError(err, text("Re-link"))
                        }
                      }}
                    >
                      <AdminMessage id="Re-link" />
                    </Button>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={onClose}>
            <AdminMessage id="Close" />
          </Button>
          {upload.step === "review" && (
            <Button
              disabled={!parsed?.rows.length || parsed.errors.length > 0}
              onClick={() => parsed && void run(parsed.rows)}
            >
              <AdminMessage id="Upload" />
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
