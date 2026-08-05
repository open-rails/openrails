import { HugeiconsIcon } from "@hugeicons/react"
import {
  Copy02Icon,
  CopyCheckIcon,
  ViewIcon,
  ViewOffIcon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { toast } from "sonner"

import { TypedConfirmDialog } from "@/components/typed-confirm-dialog"
import { Badge } from "@/components/ui/badge"
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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { useApiData } from "@/hooks/use-api-data"
import { createApiKey, listApiKeys, revokeApiKey } from "@/lib/api/endpoints"
import type { MerchantAPIKey, MintedAPIKey } from "@/lib/api/types"
import { cn } from "@/lib/utils"
import { formatDate } from "@/lib/format"
import { toastApiError } from "@/lib/toast"

// Fixed merchant catalog roles (#567) a key can hold, least privilege first.
// The API validates against the same catalog; these descriptions are the
// plain-language contract of each role.
const ROLES: { value: string; label: string; description: string }[] = [
  {
    value: "viewer",
    label: "Viewer — read-only (recommended)",
    description:
      "Can query metrics and read payments, subscriptions, catalog, and settings. " +
      "Cannot change anything or move money. The right choice for LLM agents, " +
      "reporting, and analytics integrations.",
  },
  {
    value: "support",
    label: "Support — customer operations",
    description:
      "Everything Viewer can, plus customer fixes: refunds, subscription changes, " +
      "and entitlement grants. Cannot change merchant settings, catalog, payment " +
      "providers, or API keys.",
  },
  {
    value: "owner",
    label: "Owner — full control",
    description:
      "Full authority over this merchant, including settings, payment providers, " +
      "and minting or revoking API keys. Only for trusted server automation.",
  },
]

export function ApiKeysTab() {
  const { data, loading, error, reload } = useApiData(() => listApiKeys(), [])
  React.useEffect(() => {
    if (error) toastApiError(error, "Load API keys")
  }, [error])

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>

  const keys = data?.data ?? []

  return (
    <div>
      <section className="grid gap-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid max-w-2xl gap-1">
            <h2 className="text-base font-semibold">API keys</h2>
            <p className="text-sm text-pretty text-muted-foreground">
              Create scoped credentials for integrations. Secrets are shown
              once.
            </p>
          </div>
          <CreateKeyDialog onDone={reload} />
        </div>
        {keys.length === 0 ? (
          <p className="py-2 text-sm text-muted-foreground">
            No API keys created.
          </p>
        ) : (
          <Table className="min-w-[52rem]">
            <TableHeader>
              <TableRow>
                <TableHead className="text-muted-foreground">Name</TableHead>
                <TableHead className="text-muted-foreground">Role</TableHead>
                <TableHead className="text-muted-foreground">Prefix</TableHead>
                <TableHead className="text-muted-foreground">Created</TableHead>
                <TableHead className="text-muted-foreground">
                  Last used
                </TableHead>
                <TableHead className="text-muted-foreground">Status</TableHead>
                <TableHead className="text-right text-muted-foreground">
                  Action
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((k) => (
                <KeyRow key={k.id} apiKey={k} onDone={reload} />
              ))}
            </TableBody>
          </Table>
        )}
      </section>
    </div>
  )
}

function keyStatus(k: MerchantAPIKey): "active" | "revoked" | "expired" {
  if (k.revoked_at) return "revoked"
  if (k.expires_at && new Date(k.expires_at).getTime() <= Date.now())
    return "expired"
  return "active"
}

function KeyRow({
  apiKey,
  onDone,
}: {
  apiKey: MerchantAPIKey
  onDone: () => void
}) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const status = keyStatus(apiKey)
  return (
    <TableRow className={status !== "active" ? "opacity-60" : undefined}>
      <TableCell className="py-3 font-medium">{apiKey.name}</TableCell>
      <TableCell className="py-3">
        <Badge variant="secondary" className="capitalize">
          {apiKey.role}
        </Badge>
      </TableCell>
      <TableCell className="py-3 font-mono text-xs text-muted-foreground">
        {apiKey.prefix}
      </TableCell>
      <TableCell className="py-3">{formatDate(apiKey.created_at)}</TableCell>
      <TableCell className="py-3">
        {apiKey.last_used_at ? formatDate(apiKey.last_used_at) : "never"}
      </TableCell>
      <TableCell className="py-3">
        {status === "active" ? (
          <Badge
            variant="secondary"
            className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
          >
            active
          </Badge>
        ) : (
          <Badge variant="secondary" className="capitalize">
            {status}
          </Badge>
        )}
      </TableCell>
      <TableCell className="py-3 text-right">
        {status === "active" && (
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setConfirmOpen(true)}
            >
              Revoke
            </Button>
            <TypedConfirmDialog
              open={confirmOpen}
              onOpenChange={setConfirmOpen}
              title={`Revoke "${apiKey.name}"?`}
              description="Every request using this key will be rejected immediately. This cannot be undone."
              confirmationWord="REVOKE"
              actionLabel="Revoke key"
              onConfirm={async () => {
                try {
                  await revokeApiKey(apiKey.id)
                  toast.success("API key revoked")
                  onDone()
                } catch (err) {
                  toastApiError(err, "Revoke API key")
                }
              }}
            />
          </>
        )}
      </TableCell>
    </TableRow>
  )
}

function CreateKeyDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [name, setName] = React.useState("")
  const [role, setRole] = React.useState("viewer")
  const [busy, setBusy] = React.useState(false)
  const [minted, setMinted] = React.useState<MintedAPIKey | null>(null)

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      setName("")
      setRole("viewer")
      setMinted(null)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger render={<Button size="sm">Create API key</Button>} />
      <DialogContent>
        {minted ? (
          <ShowOnceSecret
            minted={minted}
            onClose={() => handleOpenChange(false)}
          />
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>Create API key</DialogTitle>
              <DialogDescription>
                Pick the least-privileged role that can do the job — you can
                always mint another key with more authority later.
              </DialogDescription>
            </DialogHeader>
            <div className="grid gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="ak-name">Name</Label>
                <Input
                  id="ak-name"
                  placeholder="e.g. metrics-agent"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="grid gap-1.5">
                <Label>Role</Label>
                <div className="grid gap-2">
                  {ROLES.map((r) => (
                    <button
                      key={r.value}
                      type="button"
                      onClick={() => setRole(r.value)}
                      aria-pressed={role === r.value}
                      className={cn(
                        "rounded-md border p-3 text-left transition-colors",
                        role === r.value
                          ? "border-primary bg-primary/5"
                          : "hover:bg-muted/50"
                      )}
                    >
                      <p className="text-sm font-medium">{r.label}</p>
                      <p className="mt-1 text-xs text-muted-foreground">
                        {r.description}
                      </p>
                    </button>
                  ))}
                </div>
              </div>
            </div>
            <DialogFooter>
              <Button
                disabled={busy || !name.trim()}
                onClick={async () => {
                  setBusy(true)
                  try {
                    const created = await createApiKey(name.trim(), role)
                    setMinted(created)
                    onDone()
                  } catch (err) {
                    toastApiError(err, "Create API key")
                  } finally {
                    setBusy(false)
                  }
                }}
              >
                {busy ? "Creating…" : "Create key"}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

function ShowOnceSecret({
  minted,
  onClose,
}: {
  minted: MintedAPIKey
  onClose: () => void
}) {
  const [copied, setCopied] = React.useState(false)
  const [revealed, setRevealed] = React.useState(false)

  const copySecret = async () => {
    try {
      await copyText(minted.secret)
      setCopied(true)
      toast.success("Key copied to clipboard")
    } catch {
      toast.error("Copy failed — reveal and copy the key manually")
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>API key created</DialogTitle>
        <DialogDescription>
          Copy the key now and store it somewhere safe.{" "}
          <span className="font-semibold text-foreground">
            You won&apos;t see this key again
          </span>{" "}
          — if it is lost, revoke it and mint a new one.
        </DialogDescription>
      </DialogHeader>
      <div className="grid gap-3">
        <p className="text-sm">
          <span className="font-medium">{minted.name}</span>{" "}
          <Badge variant="secondary">{minted.role}</Badge>
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <code className="min-w-0 flex-1 overflow-x-auto rounded-md border bg-muted px-3 py-2 font-mono text-xs whitespace-nowrap">
            {revealed ? minted.secret : `${minted.prefix}_••••••••••••••••`}
          </code>
          <Button
            variant="outline"
            size="sm"
            aria-pressed={revealed}
            onClick={() => setRevealed((current) => !current)}
          >
            <HugeiconsIcon
              icon={revealed ? ViewOffIcon : ViewIcon}
              className="size-4"
            />
            {revealed ? "Hide" : "Show"}
          </Button>
          <Button variant="outline" size="sm" onClick={copySecret}>
            <HugeiconsIcon
              icon={copied ? CopyCheckIcon : Copy02Icon}
              className="size-4"
            />
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          Use it as a Bearer token:{" "}
          <code>Authorization: Bearer {minted.prefix}…</code>
        </p>
      </div>
      <DialogFooter>
        <Button onClick={onClose}>Done</Button>
      </DialogFooter>
    </>
  )
}

async function copyText(value: string): Promise<void> {
  if (navigator.clipboard?.writeText && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(value)
      return
    } catch {
      // Fall back for browsers that expose the API but deny clipboard access.
    }
  }

  const textarea = document.createElement("textarea")
  textarea.value = value
  textarea.readOnly = true
  textarea.style.position = "fixed"
  textarea.style.opacity = "0"
  document.body.appendChild(textarea)
  textarea.select()

  try {
    if (!document.execCommand("copy")) throw new Error("Copy failed")
  } finally {
    textarea.remove()
  }
}
