import { HugeiconsIcon } from "@hugeicons/react"
import {
  Copy02Icon,
  CopyCheckIcon,
  ViewIcon,
  ViewOffIcon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { TypedConfirmDialog } from "@/components/typed-confirm-dialog"
import { FormFieldErrors } from "@/components/form-field-errors"
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
import type { MerchantAPIKey, MintedAPIKey } from "@/lib/api/types"
import { DIALOG_FORM } from "@/lib/dialog-width"
import { cn } from "@/lib/utils"
import { formatDate } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"

// Fixed merchant catalog roles (#567) a key can hold, least privilege first.
// The API validates against the same catalog; these descriptions are the
// plain-language contract of each role.
const ROLES: { value: string; label: string; description: string }[] = [
  {
    value: "viewer",
    label: "Viewer (read-only, recommended)",
    description:
      "Can query metrics and read payments, subscriptions, catalog, and settings. " +
      "Cannot change anything or move money. The right choice for reporting and " +
      "analytics integrations.",
  },
  {
    value: "support",
    label: "Support (customer operations)",
    description:
      "Everything Viewer can, plus customer fixes: refunds, subscription changes " +
      "and entitlement grants. Cannot change merchant settings, catalog, payment " +
      "providers, or API keys.",
  },
  {
    value: "owner",
    label: "Owner (full control)",
    description:
      "Full authority over this merchant, including settings, payment providers " +
      "and API keys. Only for software you fully trust.",
  },
]

export function ApiKeysTab() {
  const { data, isPending: loading } = useQuery(adminQueries.apiKeys())

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
          <CreateKeyDialog />
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
                <KeyRow key={k.id} apiKey={k} />
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

function KeyRow({ apiKey }: { apiKey: MerchantAPIKey }) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const revokeKey = useMutation(adminMutations.revokeApiKey(queryClient))
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
            className="bg-settled-surface text-settled"
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
                  await revokeKey.mutateAsync(apiKey.id)
                  toast.success("API key revoked")
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

function CreateKeyDialog() {
  const [open, setOpen] = React.useState(false)
  const [minted, setMinted] = React.useState<MintedAPIKey | null>(null)
  const queryClient = useQueryClient()
  const createKey = useMutation(adminMutations.createApiKey(queryClient))
  const form = useForm({
    defaultValues: { name: "", role: "viewer" },
    onSubmit: async ({ value }) => {
      try {
        const created = await createKey.mutateAsync({
          name: value.name.trim(),
          role: value.role,
        })
        setMinted(created)
      } catch (err) {
        toastApiError(err, "Create API key")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      form.reset()
      setMinted(null)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger render={<Button size="sm">Create API key</Button>} />
      <DialogContent className={DIALOG_FORM}>
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
                A key lets your own software act on this account. Give it the
                smallest role that does the job. You can create another key with
                more access at any time.
              </DialogDescription>
            </DialogHeader>
            <form
              onSubmit={(event) => {
                event.preventDefault()
                event.stopPropagation()
                void form.handleSubmit()
              }}
              className="grid gap-4"
            >
              <div className="grid gap-3">
                <form.Field
                  name="name"
                  validators={{
                    onChange: ({ value }) =>
                      value.trim() ? undefined : "Enter a key name",
                  }}
                >
                  {(field) => (
                    <div className="grid gap-1.5">
                      <Label htmlFor="ak-name">Name</Label>
                      <Input
                        id="ak-name"
                        placeholder="e.g. metrics-agent"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(event) =>
                          field.handleChange(event.target.value)
                        }
                        aria-invalid={field.state.meta.errors.length > 0}
                      />
                      <FormFieldErrors errors={field.state.meta.errors} />
                    </div>
                  )}
                </form.Field>
                <form.Field name="role">
                  {(field) => (
                    <div className="grid gap-1.5">
                      <Label>Role</Label>
                      <div className="grid gap-2">
                        {ROLES.map((role) => (
                          <button
                            key={role.value}
                            type="button"
                            onClick={() => field.handleChange(role.value)}
                            aria-pressed={field.state.value === role.value}
                            className={cn(
                              "rounded-md border p-3 text-left transition-colors",
                              field.state.value === role.value
                                ? "border-primary bg-primary/5"
                                : "hover:bg-muted/50"
                            )}
                          >
                            <p className="text-sm font-medium">{role.label}</p>
                            <p className="mt-1 text-xs text-muted-foreground">
                              {role.description}
                            </p>
                          </button>
                        ))}
                      </div>
                    </div>
                  )}
                </form.Field>
              </div>
              <DialogFooter>
                <form.Subscribe
                  selector={(state) =>
                    [
                      state.values.name,
                      state.canSubmit,
                      state.isSubmitting,
                    ] as const
                  }
                >
                  {([name, canSubmit, isSubmitting]) => (
                    <>
                      <Button
                        type="button"
                        variant="outline"
                        disabled={isSubmitting}
                        onClick={() => handleOpenChange(false)}
                      >
                        Cancel
                      </Button>
                      <Button
                        type="submit"
                        disabled={!name.trim() || !canSubmit || isSubmitting}
                      >
                        {isSubmitting ? "Creating…" : "Create key"}
                      </Button>
                    </>
                  )}
                </form.Subscribe>
              </DialogFooter>
            </form>
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
      toast.error("Copy failed. Reveal the key and copy it manually.")
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
