import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  Delete02Icon,
  Mail01Icon,
  WebhookIcon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { FormFieldErrors } from "@/components/form-field-errors"
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { DIALOG_FORM } from "@/lib/dialog-width"
import type {
  MerchantSettings,
  MerchantWebhook,
  WebhookFormat,
} from "@/lib/api/types"
import { toastApiError } from "@/lib/toast"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"

// --- Page ------------------------------------------------------------------

export function NotificationsTab() {
  const settingsQuery = useQuery(adminQueries.merchantSettings("Load settings"))
  const webhooksQuery = useQuery(adminQueries.webhooks())

  const hooks = webhooksQuery.data?.data ?? []

  return (
    <div className="flex flex-col gap-10">
      <NotificationEmailSection
        key={settingsQuery.data?.alert_email ?? "∅"}
        settings={settingsQuery.data ?? undefined}
        loading={settingsQuery.isPending}
      />
      <WebhooksSection webhooks={hooks} loading={webhooksQuery.isPending} />
    </div>
  )
}

// --- Alert email -----------------------------------------------------------

function NotificationEmailSection({
  settings,
  loading,
}: {
  settings?: MerchantSettings
  loading: boolean
}) {
  const initial = settings?.alert_email ?? ""
  const queryClient = useQueryClient()
  const updateSettings = useMutation(
    adminMutations.updateMerchantSettings(queryClient)
  )
  const form = useForm({
    defaultValues: { email: initial },
    onSubmit: async ({ value }) => {
      try {
        await updateSettings.mutateAsync({
          ...(settings ?? {}),
          alert_email: value.email.trim() || undefined,
        })
        form.reset(value)
        toast.success(
          value.email.trim() ? "Alert email saved" : "Alert email cleared"
        )
      } catch (err) {
        toastApiError(err, "Save alert email")
      }
    },
  })

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">Email delivery</h2>
          <p className="text-sm text-muted-foreground">
            Send email alerts to this address.
          </p>
        </div>
        <form.Subscribe
          selector={(state) => [
            state.canSubmit,
            state.isSubmitting,
            state.isDefaultValue,
          ]}
        >
          {([canSubmit, isSubmitting, isDefaultValue]) => (
            <Button
              type="submit"
              size="sm"
              form="alert-email-form"
              disabled={loading || !canSubmit || isSubmitting || isDefaultValue}
            >
              {isSubmitting ? "Saving…" : "Save"}
            </Button>
          )}
        </form.Subscribe>
      </div>
      <form
        id="alert-email-form"
        onSubmit={(event) => {
          event.preventDefault()
          event.stopPropagation()
          void form.handleSubmit()
        }}
      >
        <form.Field name="email">
          {(field) => (
            <div className="grid gap-2 md:grid-cols-[11rem_minmax(0,1fr)] md:items-center md:gap-6">
              <Label htmlFor="alert-email">Alert email</Label>
              <div className="grid min-w-0 gap-1.5">
                <Input
                  id="alert-email"
                  type="email"
                  placeholder="alerts@example.com"
                  value={field.state.value}
                  disabled={loading}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                />
              </div>
            </div>
          )}
        </form.Field>
      </form>
    </section>
  )
}

// --- Webhooks --------------------------------------------------------------

const WEBHOOK_FORMATS: { value: WebhookFormat; label: string }[] = [
  { value: "generic", label: "Generic (your own receiver)" },
  { value: "discord", label: "Discord" },
  { value: "slack", label: "Slack" },
]

function WebhooksSection({
  webhooks,
  loading,
}: {
  webhooks: MerchantWebhook[]
  loading: boolean
}) {
  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid max-w-2xl gap-1">
          <h2 className="text-base font-semibold">Webhooks</h2>
          <p className="text-sm text-pretty text-muted-foreground">
            Send alerts to Discord, Slack, or your own endpoint.
          </p>
        </div>
        <WebhookDialog />
      </div>
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : webhooks.length === 0 ? (
        <p className="py-2 text-sm text-muted-foreground">
          No webhooks configured.
        </p>
      ) : (
        <Table className="min-w-[36rem]">
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="text-muted-foreground">Name</TableHead>
              <TableHead className="text-muted-foreground">Format</TableHead>
              <TableHead className="text-muted-foreground">URL</TableHead>
              <TableHead className="text-right text-muted-foreground">
                Action
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {webhooks.map((w) => (
              <WebhookRow key={w.id} webhook={w} />
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}

function WebhookRow({ webhook }: { webhook: MerchantWebhook }) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const removeWebhook = useMutation(adminMutations.deleteWebhook(queryClient))
  return (
    <TableRow className={webhook.enabled === false ? "opacity-60" : undefined}>
      <TableCell className="py-3 font-medium">{webhook.name}</TableCell>
      <TableCell className="py-3">
        <Badge variant="secondary">{webhook.format}</Badge>
      </TableCell>
      <TableCell
        className="max-w-[22rem] truncate py-3 text-xs text-muted-foreground"
        title={webhook.destination_host}
      >
        {webhook.destination_host}
      </TableCell>
      <TableCell className="py-3 text-right">
        <div className="flex justify-end gap-1">
          <WebhookDialog webhook={webhook} />
          <Button
            variant="ghost"
            size="icon"
            aria-label="Delete webhook"
            onClick={() => setConfirmOpen(true)}
          >
            <HugeiconsIcon
              icon={Delete02Icon}
              className="size-4 text-muted-foreground"
            />
          </Button>
          <TypedConfirmDialog
            open={confirmOpen}
            onOpenChange={setConfirmOpen}
            title={`Delete "${webhook.name}"?`}
            description="Operational notifications will stop delivering to this webhook. This cannot be undone."
            confirmationWord="DELETE"
            actionLabel="Delete webhook"
            onConfirm={async () => {
              try {
                await removeWebhook.mutateAsync(webhook.id)
                toast.success("Webhook deleted")
              } catch (err) {
                toastApiError(err, "Delete webhook")
              }
            }}
          />
        </div>
      </TableCell>
    </TableRow>
  )
}

function WebhookDialog({ webhook }: { webhook?: MerchantWebhook }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const addWebhook = useMutation(adminMutations.createWebhook(queryClient))
  const rotateWebhook = useMutation(
    adminMutations.rotateWebhookURL(queryClient)
  )
  const form = useForm({
    defaultValues: {
      name: webhook?.name ?? "",
      url: "",
      format: webhook?.format ?? ("generic" as WebhookFormat),
    },
    onSubmit: async ({ value }) => {
      try {
        if (webhook)
          await rotateWebhook.mutateAsync({
            id: webhook.id,
            url: value.url.trim(),
          })
        else
          await addWebhook.mutateAsync({
            name: value.name.trim(),
            url: value.url.trim(),
            format: value.format,
          })
        toast.success(webhook ? "Webhook URL replaced" : "Webhook added")
        handleOpen(false)
      } catch (err) {
        toastApiError(err, webhook ? "Replace webhook URL" : "Add webhook")
      }
    },
  })

  const handleOpen = (next: boolean) => {
    setOpen(next)
    if (!next) form.reset()
  }

  return (
    <Dialog open={open} onOpenChange={handleOpen}>
      <DialogTrigger
        render={
          <Button size="sm" variant="outline">
            {!webhook && <HugeiconsIcon icon={Add01Icon} className="size-4" />}{" "}
            {webhook ? "Replace URL" : "Add webhook"}
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            {webhook
              ? `Replace URL for ${webhook.name || webhook.destination_host}`
              : "Add webhook"}
          </DialogTitle>
          <DialogDescription>
            {webhook
              ? "Paste a replacement URL. Existing operational notifications keep their connection."
              : "Paste a webhook address from Discord, Slack, or your own alert receiver."}
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
                  webhook || value.trim() ? undefined : "Enter a webhook name",
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="wh-name">Name</Label>
                  <Input
                    id="wh-name"
                    disabled={Boolean(webhook)}
                    placeholder="e.g. #billing-alerts"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
            <form.Field name="format">
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="wh-format">Format</Label>
                  <Select
                    disabled={Boolean(webhook)}
                    value={field.state.value}
                    onValueChange={(value) =>
                      field.handleChange(value as WebhookFormat)
                    }
                  >
                    <SelectTrigger id="wh-format" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {WEBHOOK_FORMATS.map((format) => (
                        <SelectItem key={format.value} value={format.value}>
                          {format.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              )}
            </form.Field>
            <form.Field
              name="url"
              validators={{
                onChange: ({ value }) => {
                  if (!value.trim()) return "Enter a webhook URL"
                  try {
                    new URL(value)
                    return undefined
                  } catch {
                    return "Enter a valid webhook URL"
                  }
                },
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="wh-url">Webhook URL</Label>
                  <Input
                    id="wh-url"
                    type="url"
                    className="text-xs"
                    placeholder="https://discord.com/api/webhooks/…"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.name,
                  state.values.url,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([name, url, canSubmit, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={
                    (!webhook && !name.trim()) ||
                    !url.trim() ||
                    !canSubmit ||
                    isSubmitting
                  }
                >
                  {isSubmitting
                    ? "Saving…"
                    : webhook
                      ? "Replace URL"
                      : "Add webhook"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
