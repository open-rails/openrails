import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon, Delete02Icon } from "@hugeicons/core-free-icons"
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
import { Switch } from "@/components/ui/switch"
import { DIALOG_FORM } from "@/lib/dialog-width"
import type {
  AlertWebhook,
  MerchantConfigurationState,
} from "@/lib/api/generated/wire"
import { useMerchantConfigEdits } from "@/lib/capabilities"
import { toastApiError } from "@/lib/toast"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { useConfigurationEdit } from "./configuration-edit"
import {
  EditActions,
  ReadOnlyNotice,
  SettingDetail,
  SettingEditField,
} from "./setting-fields"

type WebhookFormat = AlertWebhook["format"]

// --- Page ------------------------------------------------------------------

export function NotificationsTab() {
  const configurationQuery = useQuery(
    adminQueries.merchantConfiguration("Load settings")
  )
  const webhooksQuery = useQuery(adminQueries.webhooks())
  const edits = useMerchantConfigEdits()

  const hooks = webhooksQuery.data?.data ?? []

  return (
    <div className="flex flex-col gap-10">
      <ReadOnlyNotice />
      {configurationQuery.data ? (
        <NotificationEmailSection
          configuration={configurationQuery.data}
          edits={edits}
        />
      ) : (
        <p className="text-sm text-muted-foreground">Loading…</p>
      )}
      <WebhooksSection
        webhooks={hooks}
        loading={webhooksQuery.isPending}
        edits={edits}
      />
    </div>
  )
}

// --- Alert email -----------------------------------------------------------

function NotificationEmailSection({
  configuration,
  edits,
}: {
  configuration: MerchantConfigurationState
  edits: boolean
}) {
  const email = configuration.settings.alert_email ?? ""
  const edit = useConfigurationEdit(configuration)
  const form = useForm({
    defaultValues: { email },
    // An empty address clears the email channel.
    onSubmit: ({ value }) =>
      edit.save(
        { settings: { alert_email: value.email.trim() } },
        value.email.trim() ? "Alert email saved" : "Alert email cleared",
        "Save alert email"
      ),
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
        <form.Subscribe selector={(state) => state.isSubmitting}>
          {(isSubmitting) => (
            <EditActions
              edits={edits}
              editing={edit.editing}
              form="alert-email-form"
              saving={isSubmitting}
              onEdit={() => {
                form.reset({ email })
                edit.open()
              }}
              onCancel={edit.close}
            />
          )}
        </form.Subscribe>
      </div>
      {edit.editing ? (
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
              <SettingEditField label="Alert email" id="alert-email">
                <Input
                  id="alert-email"
                  type="email"
                  placeholder="alerts@example.com"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                />
              </SettingEditField>
            )}
          </form.Field>
        </form>
      ) : (
        <dl>
          <SettingDetail label="Alert email" value={email} />
        </dl>
      )}
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
  edits,
}: {
  webhooks: AlertWebhook[]
  loading: boolean
  edits: boolean
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
        {edits && <WebhookDialog />}
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
              {edits && (
                <TableHead className="text-right text-muted-foreground">
                  Action
                </TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {webhooks.map((w) => (
              <WebhookRow key={w.id} webhook={w} edits={edits} />
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}

function WebhookRow({
  webhook,
  edits,
}: {
  webhook: AlertWebhook
  edits: boolean
}) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const removeWebhook = useMutation(adminMutations.deleteWebhook(queryClient))
  const name = webhook.name ?? webhook.destination_host
  return (
    <TableRow className={webhook.enabled ? undefined : "opacity-60"}>
      <TableCell className="py-3 font-medium">{name}</TableCell>
      <TableCell className="py-3">
        <span className="flex flex-wrap gap-1">
          <Badge variant="secondary">{webhook.format}</Badge>
          {!webhook.enabled && <Badge variant="outline">disabled</Badge>}
        </span>
      </TableCell>
      <TableCell
        className="max-w-[22rem] truncate py-3 text-xs text-muted-foreground"
        title={webhook.destination_host}
      >
        {webhook.destination_host}
      </TableCell>
      {edits && (
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
              title={`Delete "${name}"?`}
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
      )}
    </TableRow>
  )
}

// WebhookDialog adds a webhook or edits one. The URL carries the receiver's
// secret, so it is write-only: an edit leaves it blank to keep it.
function WebhookDialog({ webhook }: { webhook?: AlertWebhook }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const addWebhook = useMutation(adminMutations.createWebhook(queryClient))
  const updateWebhook = useMutation(adminMutations.updateWebhook(queryClient))
  const initial = () => ({
    name: webhook?.name ?? "",
    url: "",
    format: webhook?.format ?? ("generic" as WebhookFormat),
    enabled: webhook?.enabled ?? true,
  })
  const form = useForm({
    defaultValues: initial(),
    onSubmit: async ({ value }) => {
      const url = value.url.trim()
      try {
        if (webhook)
          await updateWebhook.mutateAsync({
            id: webhook.id,
            webhook: {
              name: value.name.trim() || null,
              format: value.format,
              enabled: value.enabled,
              ...(url ? { url } : {}),
            },
          })
        else
          await addWebhook.mutateAsync({
            name: value.name.trim(),
            url,
            format: value.format,
          })
        toast.success(webhook ? "Webhook saved" : "Webhook added")
        handleOpen(false)
      } catch (err) {
        toastApiError(err, webhook ? "Save webhook" : "Add webhook")
      }
    },
  })

  const handleOpen = (next: boolean) => {
    if (next) form.reset(initial())
    else form.reset()
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpen}>
      <DialogTrigger
        render={
          <Button size="sm" variant="outline">
            {!webhook && <HugeiconsIcon icon={Add01Icon} className="size-4" />}{" "}
            {webhook ? "Edit" : "Add webhook"}
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            {webhook
              ? `Edit ${webhook.name ?? webhook.destination_host}`
              : "Add webhook"}
          </DialogTitle>
          <DialogDescription>
            {webhook
              ? "The current URL is never shown. Paste a new one to replace it, or leave it blank to keep it."
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
                  if (!value.trim())
                    return webhook ? undefined : "Enter a webhook URL"
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
                    autoComplete="off"
                    className="text-xs"
                    placeholder={
                      webhook
                        ? "unchanged"
                        : "https://discord.com/api/webhooks/…"
                    }
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
            {webhook && (
              <form.Field name="enabled">
                {(field) => (
                  <div className="flex items-center gap-2">
                    <Switch
                      id="wh-enabled"
                      checked={field.state.value}
                      onCheckedChange={field.handleChange}
                    />
                    <Label htmlFor="wh-enabled">Enabled</Label>
                  </div>
                )}
              </form.Field>
            )}
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.name,
                  state.values.url,
                  state.isDirty,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([name, url, isDirty, canSubmit, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={
                    (webhook ? !isDirty : !name.trim() || !url.trim()) ||
                    !canSubmit ||
                    isSubmitting
                  }
                >
                  {isSubmitting ? "Saving…" : webhook ? "Save" : "Add webhook"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
