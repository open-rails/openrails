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
import { ApiError, getTokens } from "@/lib/api/client"
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
  const [currentRevision, setCurrentRevision] = React.useState<number>()
  const [draftMerchant, setDraftMerchant] = React.useState<string>()
  const apply = useMutation(adminMutations.applyCatalog(useQueryClient()))

  const start = async () => {
    const selectedMerchant = getTokens()?.merchant
    setLoading(true)
    try {
      const { revision, writes_allowed } = await getCatalogRevision()
      if (getTokens()?.merchant !== selectedMerchant) {
        setMessage(
          "The selected merchant changed. Load its catalog revision before preparing an application."
        )
        return
      }
      if (!writes_allowed) {
        setMessage(
          "Catalog updates are disabled. The current catalog remains available for reading."
        )
        return
      }
      setDraftMerchant(selectedMerchant)
      setDocument(
        JSON.stringify(
          {
            schema_version: 1,
            application_id: crypto.randomUUID(),
            expected_revision: revision,
            prune: false,
            products: [],
          },
          null,
          2
        )
      )
      setCurrentRevision(revision)
      setSubmitted(undefined)
      setReceipt(undefined)
      setReviewed(false)
      setCanEdit(false)
      setMessage("")
    } catch (err) {
      setMessage(
        "Could not load the catalog revision. Retry loading before preparing an application."
      )
      toastApiError(err, "Load catalog revision")
    } finally {
      setLoading(false)
    }
  }

  const reviewCurrentRevision = async () => {
    setLoading(true)
    try {
      const { revision } = await getCatalogRevision()
      setCurrentRevision(revision)
      // Reading current state never changes the submitted precondition or ID.
      setMessage(
        `Current revision: ${revision}. Review the catalog, then edit the document with a new application ID and the intended revision.`
      )
    } catch (err) {
      toastApiError(err, "Load catalog revision")
    } finally {
      setLoading(false)
    }
  }

  const run = async () => {
    if (getTokens()?.merchant !== draftMerchant) {
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
          ? "This application was already applied. No changes were repeated. Review the current catalog before starting a new application."
          : "Application committed. Review the current catalog before starting another application."
      )
      toast.success(
        result.replayed
          ? "Original application receipt returned"
          : "Catalog application committed"
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
          ? "Application refused. Review the current catalog and document before editing. The application ID and expected revision have not changed."
          : "The outcome could not be confirmed. Retry the exact application to retrieve its result; its ID, revision and contents are preserved."
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
            Paste a complete JSON or YAML application. Omitted records stay
            unchanged unless prune is true. With prune, omitted prices are
            archived even under a listed product. Keep the application ID and
            expected revision unchanged when retrying.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <Label htmlFor="catalog-application">Application document</Label>
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
          {currentRevision !== undefined && (
            <p className="text-sm text-muted-foreground">
              Last loaded catalog revision: {currentRevision}
            </p>
          )}
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
              aria-label="Application receipt"
              className="max-h-48 overflow-auto rounded-md border p-3 text-xs"
            >
              {JSON.stringify(receipt, null, 2)}
            </pre>
          )}
        </div>
        <DialogFooter>
          {!document && (
            <Button disabled={loading} onClick={() => void start()}>
              Load catalog revision
            </Button>
          )}
          {canEdit && (
            <>
              <Button
                variant="outline"
                disabled={loading}
                onClick={() => void reviewCurrentRevision()}
              >
                Review current revision
              </Button>
              <Button
                variant="outline"
                onClick={() => {
                  setSubmitted(undefined)
                  setReviewed(false)
                  setCanEdit(false)
                }}
              >
                Edit application
              </Button>
            </>
          )}
          {receipt ? (
            <Button disabled={loading} onClick={() => void start()}>
              New application
            </Button>
          ) : submitted !== undefined ? (
            <Button
              disabled={apply.isPending || loading || canEdit}
              onClick={() => void run()}
            >
              {apply.isPending ? "Applying…" : "Retry exact application"}
            </Button>
          ) : reviewed ? (
            <Button
              disabled={loading || !document.trim()}
              onClick={() => void run()}
            >
              Apply reviewed application
            </Button>
          ) : (
            <Button
              disabled={loading || !document.trim()}
              onClick={() => setReviewed(true)}
            >
              Review application
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
