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
import {
  getCatalogRevision,
  type CatalogApplicationReceipt,
} from "@/lib/api/endpoints"
import { DIALOG_WIDE } from "@/lib/dialog-width"
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
  const [loading, setLoading] = React.useState(false)
  const [draftMerchant, setDraftMerchant] = React.useState<string>()
  const apply = useMutation(adminMutations.applyCatalog(useQueryClient()))

  const start = async () => {
    const merchant = selectedMerchant()
    setLoading(true)
    try {
      const { writes_allowed } = await getCatalogRevision()
      if (selectedMerchant() !== merchant) {
        setMessage(
          "The selected merchant changed. Reload its catalog access before preparing a batch."
        )
        return
      }
      if (!writes_allowed) {
        setMessage(
          "Catalog updates are disabled. The current catalog remains available for reading."
        )
        return
      }
      setDraftMerchant(merchant)
      setDocument(
        JSON.stringify(
          {
            schema_version: 1,
            prune: false,
            products: [],
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
    } catch (err) {
      setMessage(
        "Could not check catalog access. Retry loading before preparing a batch."
      )
      toastApiError(err, "Check catalog access")
    } finally {
      setLoading(false)
    }
  }

  const run = async () => {
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
      const result = await apply.mutateAsync(exactDocument)
      setReceipt(result)
      setMessage(
        result.replayed
          ? "This batch was already applied. No changes were repeated, and later catalog edits were preserved."
          : "Batch applied. Review the current catalog before preparing another batch."
      )
      toast.success(
        result.replayed
          ? "Original batch receipt returned"
          : "Catalog batch applied"
      )
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
        if (apply.isPending || loading) return
        setOpen(next)
        if (next && !document) void start()
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
            Paste a JSON or YAML catalog batch. Each distinct batch applies once
            per merchant; retries preserve later catalog edits. Omitted records
            stay unchanged unless prune is true. With prune, omitted products
            and prices are archived, including prices under a listed product.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <Label htmlFor="catalog-application">Catalog document</Label>
          <Textarea
            id="catalog-application"
            className="max-h-80 min-h-48 font-mono text-xs"
            spellCheck={false}
            value={document}
            disabled={loading || submitted !== undefined}
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
          {!document && (
            <Button disabled={loading} onClick={() => void start()}>
              Check catalog access
            </Button>
          )}
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
          {receipt ? (
            <Button disabled={loading} onClick={() => void start()}>
              New batch
            </Button>
          ) : submitted !== undefined ? (
            <Button
              disabled={apply.isPending || loading || canEdit}
              onClick={() => void run()}
            >
              {apply.isPending ? "Applying…" : "Retry exact batch"}
            </Button>
          ) : reviewed ? (
            <Button
              disabled={loading || !document.trim()}
              onClick={() => void run()}
            >
              Apply reviewed batch
            </Button>
          ) : (
            <Button
              disabled={loading || !document.trim()}
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
