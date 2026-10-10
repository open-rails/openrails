import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { ApiError, selectedMerchant } from "@/lib/api/client"
import { type CatalogApplicationReceipt } from "@/lib/api/endpoints"
import type {
  CatalogConflict,
  CatalogFieldConflict,
} from "@/lib/api/generated/wire"
import { DIALOG_WIDE } from "@/lib/dialog-width"
import { formatHours } from "@/lib/duration"
import { formatDate, formatNativeAmount } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"

export function CatalogApplicationDialog() {
  const [open, setOpen] = React.useState(false)
  const [document, setDocument] = React.useState("")
  const [reviewed, setReviewed] = React.useState(false)
  const [submitted, setSubmitted] = React.useState<string>()
  const [receipt, setReceipt] = React.useState<CatalogApplicationReceipt>()
  const [message, setMessage] = React.useState("")
  const [canEdit, setCanEdit] = React.useState(false)
  const [draftMerchant, setDraftMerchant] = React.useState<string>()
  const apply = useMutation(adminMutations.applyCatalog(useQueryClient()))

  const start = () => {
    setDraftMerchant(selectedMerchant())
    setDocument(
      JSON.stringify(
        {
          schema_version: 1,
          prune: false,
          products: {},
        },
        null,
        2
      )
    )
    setSubmitted(undefined)
    setReceipt(undefined)
    setReviewed(false)
    setCanEdit(false)
    setMessage("")
  }

  const run = async (force = false) => {
    if (selectedMerchant() !== draftMerchant) {
      setMessage(
        "This draft belongs to another selected merchant. Return to that merchant before applying it."
      )
      return
    }
    const exactDocument = submitted ?? document
    setSubmitted(exactDocument)
    setCanEdit(false)
    setMessage("")
    try {
      const result = await apply.mutateAsync({ document: exactDocument, force })
      setReceipt(result)
      const skipped = result.conflicts?.length ?? 0
      setMessage(
        result.replayed
          ? "This batch was already applied. No changes were repeated, and later catalog edits were preserved."
          : skipped > 0
            ? `Applied everything no edit changed. ${skipped} ${skipped === 1 ? "object was" : "objects were"} skipped because an edit set ${skipped === 1 ? "its" : "their"} fields differently: change the batch to agree, remove those fields, or overwrite the edits.`
            : "Batch applied. Review the current catalog before preparing another batch."
      )
      if (skipped > 0) {
        toast.warning(`Catalog batch applied; ${skipped} skipped`)
      } else {
        toast.success(
          result.replayed
            ? "Original batch receipt returned"
            : "Catalog batch applied"
        )
      }
    } catch (err) {
      const refused =
        err instanceof ApiError &&
        err.status >= 400 &&
        err.status < 500 &&
        err.status !== 408
      setCanEdit(refused)
      setMessage(
        refused
          ? "Batch refused. Review the error and edit the document before trying again."
          : "The outcome could not be confirmed. Retry this exact document to retrieve its receipt without repeating changes."
      )
      toastApiError(err, "Apply catalog")
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (apply.isPending) return
        setOpen(next)
        if (next && !document) start()
      }}
    >
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            Apply catalog
          </Button>
        }
      />
      <DialogContent className={DIALOG_WIDE}>
        <DialogHeader>
          <DialogTitle>Apply catalog changes</DialogTitle>
          <DialogDescription>
            Paste a JSON or YAML catalog batch; products, prices and meters are
            maps keyed by their key. Each product, price and meter applies
            whole, unless a field it names was set differently by an edit: it
            is skipped and listed, and the rest applies. A batch that applied
            whole is remembered; retries preserve later catalog edits. Omitted
            records stay unchanged unless prune is true, which archives the
            omitted ones only batches set.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <Label htmlFor="catalog-application">Catalog document</Label>
          <Textarea
            id="catalog-application"
            className="max-h-80 min-h-48 font-mono text-xs"
            spellCheck={false}
            value={document}
            disabled={submitted !== undefined}
            onChange={(event) => {
              setDocument(event.target.value)
              setReviewed(false)
            }}
          />
          {reviewed && !submitted && (
            <p className="text-sm">
              Review complete. Applying sends this exact document; this is not a
              database diff.
            </p>
          )}
          {message && (
            <p role="status" className="text-sm">
              {message}
            </p>
          )}
          {receipt && receipt.conflicts.length > 0 && (
            <CatalogConflicts conflicts={receipt.conflicts} />
          )}
          {receipt && (
            <pre
              aria-label="Batch receipt"
              className="max-h-48 overflow-auto rounded-md border p-3 text-xs"
            >
              {JSON.stringify(receipt, null, 2)}
            </pre>
          )}
        </div>
        <DialogFooter>
          {canEdit && (
            <Button
              variant="outline"
              onClick={() => {
                setSubmitted(undefined)
                setReviewed(false)
                setCanEdit(false)
                setMessage("")
              }}
            >
              Edit batch
            </Button>
          )}
          {receipt && receipt.conflicts.length > 0 && (
            <Button
              variant="destructive"
              disabled={apply.isPending}
              onClick={() => void run(true)}
            >
              Overwrite edits
            </Button>
          )}
          {receipt ? (
            <Button onClick={() => start()}>
              New batch
            </Button>
          ) : submitted !== undefined ? (
            <Button
              disabled={apply.isPending || canEdit}
              onClick={() => void run()}
            >
              {apply.isPending ? "Applying…" : "Retry exact batch"}
            </Button>
          ) : reviewed ? (
            <Button
              disabled={!document.trim()}
              onClick={() => void run()}
            >
              Apply reviewed batch
            </Button>
          ) : (
            <Button
              disabled={!document.trim()}
              onClick={() => setReviewed(true)}
            >
              Review batch
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// CatalogConflicts lists each object a batch skipped and the fields an edit
// set differently, money and durations in readable units.
function CatalogConflicts({ conflicts }: { conflicts: CatalogConflict[] }) {
  return (
    <ul aria-label="Skipped objects" className="grid gap-2 text-sm">
      {conflicts.map((conflict) => (
        <li
          key={`${conflict.object}:${conflict.product_key ?? ""}:${conflict.key}`}
          className="rounded-md border p-2"
        >
          <p className="font-medium">
            {conflict.product_key
              ? `Price ${conflict.key} of product ${conflict.product_key}`
              : `${conflict.object === "meter" ? "Meter" : "Product"} ${conflict.key}`}{" "}
            skipped
          </p>
          <ul className="mt-1 grid gap-1 text-xs text-muted-foreground">
            {conflict.fields.map((field) => (
              <li key={field.field}>
                {field.field}: the batch says{" "}
                {readableValue(field, field.file_value, conflict.currency)}, an
                edit set {readableValue(field, field.live_value, conflict.currency)}{" "}
                ({field.set_by}, {formatDate(field.set_at)})
              </li>
            ))}
          </ul>
        </li>
      ))}
    </ul>
  )
}

function readableValue(
  field: CatalogFieldConflict,
  value: unknown,
  currency: string | null
): string {
  if (value === null || value === undefined) return "none"
  if (
    (field.field === "unit_amount" || field.field === "trial_unit_amount") &&
    typeof value === "string" &&
    currency
  ) {
    return formatNativeAmount(value, currency)
  }
  if (field.field.endsWith("_hours") && typeof value === "number") {
    return formatHours(value)
  }
  return JSON.stringify(value)
}
