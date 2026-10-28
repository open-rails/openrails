import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useNavigate } from "#orck/admin/router"

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
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import { formatDate } from "#orck/admin/lib/format"
import type {
  PSP,
  PSPImport,
  PSPImportRule,
  ProviderCatalogTerms,
} from "#orck/client/generated/wire"
import { importProgress, successor } from "./progress"

export const selectClass = "h-8 w-full rounded-md border bg-background px-2"

// The rails a PSP import reads.
export const IMPORT_RAILS = ["nmi", "stripe", "ccbill"]

// usePSPs is the merchant's PSPs, with a lookup by id.
export function usePSPs() {
  const { adminQueries } = useAdminServices()
  const { data } = useQuery(adminQueries.psps())
  return React.useMemo(() => {
    const list = data?.data ?? []
    return { list, byId: new Map(list.map((p) => [p.id, p])) }
  }, [data])
}

export function ruleText(rule: PSPImportRule) {
  return [
    rule.fact,
    rule.pattern ? `matching ${rule.pattern}` : "",
    "→",
    rule.via,
    rule.mapping ?? "",
  ]
    .filter(Boolean)
    .join(" ")
}

export function termsText(terms: ProviderCatalogTerms | null, rail?: string) {
  if (!terms) return "—"
  const amount = terms.stated_amount
    ? `${terms.stated_amount} ${terms.currency ?? (rail === "nmi" ? "USD" : "")}`.trim()
    : "no fixed amount"
  const every = terms.billing_interval_months
    ? `${terms.billing_interval_months} mo`
    : terms.billing_interval_hours
      ? terms.billing_interval_hours % 24 === 0
        ? `${terms.billing_interval_hours / 24} d`
        : `${terms.billing_interval_hours} h`
      : ""
  return every ? `${amount} / ${every}` : amount
}

// ProgressText says where the import's job is.
export function ProgressText({ imp }: { imp: PSPImport }) {
  const { text } = useAdminUI()
  const p = importProgress(imp)
  switch (p.phase) {
    case "reading":
      return <>{text("Reading catalog and roster")}</>
    case "history":
      return (
        <>
          {text("History through ")}
          {formatDate(p.through)}
          {p.percent !== undefined && ` (${p.percent}%)`}
        </>
      )
    case "linking":
      return <>{text("Linking")}</>
    case "landing":
      return <>{text("Landing")}</>
    case "pulled":
      return <>{text("Pulled: ready to apply")}</>
    case "applied":
      return <>{text("Applied")}</>
    case "failed":
      return <>{imp.error ?? text("Failed")}</>
  }
}

const HISTORY = [
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "date", label: "Since a date" },
  { value: "full", label: "Full history" },
]

// ReconcileDialog pulls one PSP again, read-only: the import stages what
// OpenRails does not hold and nothing lands before Apply.
export function ReconcileDialog({
  psp: initial,
  onClose,
}: {
  psp?: string
  onClose: () => void
}) {
  const { text: adminText, id: adminId } = useAdminUI()
  const { importQueries, importMutations, toast, toastApiError } =
    useAdminServices()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const psps = usePSPs().list.filter(
    (p) => !p.archived && IMPORT_RAILS.includes(p.rail)
  )
  const [psp, setPSP] = React.useState(initial ?? "")
  const chosen = psp || psps[0]?.id || ""
  const [history, setHistory] = React.useState("90")
  const [date, setDate] = React.useState("")
  const latest = useQuery(importQueries.latest(chosen)).data
  const create = useMutation(importMutations.create(queryClient))
  const since = (): string | undefined => {
    if (history === "full") return undefined
    if (history === "date") return new Date(`${date}T00:00:00Z`).toISOString()
    return new Date(Date.now() - Number(history) * 86_400_000).toISOString()
  }
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id="Reconcile a PSP" />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage id="Reads the PSP's catalog, customers, subscriptions and history again, read-only. Nothing lands before you apply it." />
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("reconcile-psp")}>
            <AdminMessage id="PSP" />
          </Label>
          <select
            id={adminId("reconcile-psp")}
            className={selectClass}
            value={chosen}
            onChange={(e) => setPSP(e.target.value)}
          >
            {psps.map((p) => (
              <option key={p.id} value={p.id}>
                {p.key} ({p.rail})
              </option>
            ))}
          </select>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("reconcile-history")}>
            <AdminMessage id="History since" />
          </Label>
          <select
            id={adminId("reconcile-history")}
            className={selectClass}
            value={history}
            onChange={(e) => setHistory(e.target.value)}
          >
            {HISTORY.map((h) => (
              <option key={h.value} value={h.value}>
                {adminText(h.label)}
              </option>
            ))}
          </select>
          {history === "date" && (
            <Input
              type="date"
              aria-label={adminText("History start date")}
              value={date}
              onChange={(e) => setDate(e.target.value)}
            />
          )}
        </div>
        <div className="text-sm">
          <p className="text-muted-foreground">
            <AdminMessage id="Links by the rules of this PSP's latest import:" />
          </p>
          <ul className="list-disc pl-5">
            {(latest?.rules ?? [{ fact: "email", via: "directory" }]).map(
              (rule, i) => (
                <li key={i}>{ruleText(rule)}</li>
              )
            )}
          </ul>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            <AdminMessage id="Cancel" />
          </Button>
          <Button
            disabled={
              !chosen || create.isPending || (history === "date" && !date)
            }
            onClick={async () => {
              try {
                const imp = await create.mutateAsync({
                  psp: { id: chosen },
                  history_since: since(),
                })
                toast.success(adminText("Reconcile started"))
                onClose()
                navigate(`/ops/imports/${imp.id}`)
              } catch (err) {
                toastApiError(err, adminText("Reconcile"))
              }
            }}
          >
            <AdminMessage id="Reconcile" />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// useSuccessor is the newer import of imp's PSP that supersedes it, if any.
export function useSuccessor(imp?: PSPImport) {
  const { importQueries } = useAdminServices()
  const { data } = useQuery({
    ...importQueries.list({ psp: imp?.psp }, 100),
    enabled: Boolean(imp),
  })
  return imp && data ? successor(imp, data.data) : undefined
}

// Superseded says a newer pull of the PSP replaces this import: only that
// one is linked and applied.
export function Superseded({ by }: { by: PSPImport }) {
  return (
    <p
      role="status"
      data-testid="superseded"
      className="border-held/40 bg-held-surface text-held rounded-md border p-3 text-sm"
    >
      <AdminMessage id="A newer import of this PSP supersedes this one: link and apply " />
      <Link className="underline" to={`/ops/imports/${by.id}`}>
        <AdminMessage id="that import" />
      </Link>
      .
    </p>
  )
}

export function pspKey(byId: Map<string, PSP>, id: string) {
  return byId.get(id)?.key ?? id
}
