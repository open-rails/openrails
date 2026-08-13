import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  Delete02Icon,
  Mail01Icon,
  Notification01Icon,
  SentIcon,
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
import { Switch } from "@/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import type { AlertRuleRequest } from "@/lib/api/endpoints"
import { DIALOG_FORM } from "@/lib/dialog-width"
import type {
  AlertChannelRef,
  AlertDeliveryResult,
  AlertRule,
  AlertSeverity,
  AlertTemplate,
  AlertTemplateInfo,
  MerchantSettings,
  MerchantWebhook,
  WebhookFormat,
} from "@/lib/api/types"
import { cn } from "@/lib/utils"
import { toastApiError } from "@/lib/toast"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"

// --- Template copy (v1 set) -------------------------------------------------
// The param SCHEMA is fetched from GET /merchant/alerts/templates (the create/
// edit dialog renders its fields from that, never a hardcoded shape). These
// are just nicer plain-language label/description overrides for the known v1
// keys — the engine's own copy (display_name/description) is the fallback for
// any template this map doesn't recognize.

const TEMPLATE_COPY: Record<string, { label: string; description: string }> = {
  chargeback_rate_by_rail_account: {
    label: "Chargeback ratio per rail account",
    description:
      "Warns you before the card networks start monitoring you. Fires when a rail account's chargeback rate nears the limit of roughly 0.9%, the point where fines begin.",
  },
  dunning_spike: {
    label: "Dunning spike",
    description:
      "Fires when the number of subscriptions with a failed payment jumps above its recent average. An early sign that a rail has started declining hard.",
  },
  payers_at_depletion_risk: {
    label: "Payers approaching credit depletion",
    description:
      "Fires when a group of prepaid customers is about to run out of credit. That is when they are most likely to top up, and most likely to leave.",
  },
  payment_methods_expiring: {
    label: "Payment methods expiring (monthly digest)",
    description:
      "A monthly list of cards expiring soon, so you can ask customers to update them before a payment fails.",
  },
  webhook_silence: {
    label: "A payment provider has gone quiet",
    description:
      "Fires when a provider you take money through stops sending updates altogether, which usually means it is pointed at the wrong address.",
  },
  webhook_rejects: {
    label: "Updates arriving but being rejected",
    description:
      "Fires when a provider's updates reach you but fail their security check. Almost always the wrong signing secret.",
  },
  webhook_drift: {
    label: "Updates arriving late or not at all",
    description:
      "Fires when changes at a provider are found by checking rather than being announced, which means some updates are quietly going missing.",
  },
}

const SEVERITY_ITEMS = [
  { value: "warning", label: "Warning (shown in the console)" },
  { value: "critical", label: "Critical (also emailed and sent to webhooks)" },
]

// The engine describes each parameter for an operator reading an API. These say
// the same thing to whoever runs the shop.
const PARAM_COPY: Record<string, string> = {
  threshold:
    "The chargeback rate that sets off the alert, as a share of payments. 0.0063 is roughly 0.63%.",
  window: "How far back to measure, such as 30d or 12w.",
  multiplier:
    "How many times the usual number counts as a spike. 2 means twice the recent average.",
  min_count: "How many have to pile up before you are told.",
  runway_days: "How few days of credit left counts as running out.",
  days_ahead: "How far ahead to look for cards that are about to expire.",
}

function paramDescription(name: string, fallback: string): string {
  return PARAM_COPY[name] ?? fallback
}

function templateLabel(t: AlertTemplateInfo): string {
  return TEMPLATE_COPY[t.key]?.label ?? t.display_name
}

function templateDescription(t: AlertTemplateInfo): string {
  return TEMPLATE_COPY[t.key]?.description ?? t.description
}

function templateByKey(
  templates: AlertTemplateInfo[],
  key: string
): AlertTemplateInfo | undefined {
  return templates.find((t) => t.key === key)
}

// humanizeParam turns a snake_case param name into a label (the schema only
// carries name/description, not a display label).
function humanizeParam(name: string): string {
  return name
    .split("_")
    .filter(Boolean)
    .map((word, index) =>
      index === 0 ? word[0].toUpperCase() + word.slice(1) : word
    )
    .join(" ")
}

// runway_days is fixed by the #733 engine (only 7 is accepted) — render it
// read-only/informational rather than let the operator pick a value the
// server will reject.
function isFixedParam(templateKey: string, paramName: string): boolean {
  return (
    templateKey === "payers_at_depletion_risk" && paramName === "runway_days"
  )
}

// --- Channel wire adapter (array is the ONE true shape) ---------------------

interface ChannelSelection {
  in_app: boolean
  email: boolean
  webhookIds: string[]
}

function channelsFromWire(
  channels: AlertChannelRef[] | undefined
): ChannelSelection {
  const sel: ChannelSelection = { in_app: false, email: false, webhookIds: [] }
  for (const c of channels ?? []) {
    if (c.type === "in_app") sel.in_app = true
    else if (c.type === "email") sel.email = true
    else if (c.type === "webhook" && c.webhook_id)
      sel.webhookIds.push(c.webhook_id)
  }
  return sel
}

// in_app is always on; email/webhooks are the operator's choice.
function channelsToWire(sel: ChannelSelection): AlertChannelRef[] {
  const out: AlertChannelRef[] = [{ type: "in_app" }]
  if (sel.email) out.push({ type: "email" })
  for (const id of sel.webhookIds) out.push({ type: "webhook", webhook_id: id })
  return out
}

// summarizeDeliveryResults turns a test-fire response into one toast line.
function summarizeDeliveryResults(results: AlertDeliveryResult[]): string {
  if (results.length === 0) return "Test alert sent (rule has no channels)"
  const failed = results.filter((r) => !r.ok)
  if (failed.length === 0) {
    return `Test alert delivered to all ${results.length} channel${results.length > 1 ? "s" : ""}`
  }
  const detail = failed.map((f) => f.detail || f.channel).join(";")
  return `Test alert: ${results.length - failed.length}/${results.length} channels succeeded (${detail})`
}

// --- Page ------------------------------------------------------------------

export function AlertsTab() {
  const settingsQuery = useQuery(adminQueries.merchantSettings("Load settings"))
  const templatesQuery = useQuery(adminQueries.alertTemplates())
  const rulesQuery = useQuery(adminQueries.alertRules())
  const webhooksQuery = useQuery(adminQueries.webhooks())

  const alertEmail = settingsQuery.data?.alert_email ?? ""
  const hooks = webhooksQuery.data?.data ?? []
  const templateList = templatesQuery.data?.data ?? []

  return (
    <div className="flex flex-col gap-10">
      <AlertEmailSection
        key={settingsQuery.data?.alert_email ?? "∅"}
        settings={settingsQuery.data ?? undefined}
        loading={settingsQuery.isPending}
      />
      <RulesSection
        rules={rulesQuery.data?.data ?? []}
        loading={rulesQuery.isPending}
        templates={templateList}
        templatesLoading={templatesQuery.isPending}
        webhooks={hooks}
        alertEmail={alertEmail}
      />
      <WebhooksSection webhooks={hooks} loading={webhooksQuery.isPending} />
    </div>
  )
}

// --- Alert email -----------------------------------------------------------

function AlertEmailSection({
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

// --- Rules -----------------------------------------------------------------

function ruleIsFiring(rule: AlertRule): boolean {
  if (!rule.fired_at) return false
  if (!rule.cleared_at) return true
  return new Date(rule.fired_at).getTime() > new Date(rule.cleared_at).getTime()
}

function SeverityBadge({ severity }: { severity: AlertSeverity }) {
  return (
    <Badge
      variant="secondary"
      className={
        severity === "critical"
          ? "bg-failed-surface text-failed"
          : "bg-held-surface text-held"
      }
    >
      {severity}
    </Badge>
  )
}

function ChannelSummary({ channels }: { channels: AlertChannelRef[] }) {
  const sel = channelsFromWire(channels)
  return (
    <span className="flex flex-wrap items-center gap-1 text-xs text-muted-foreground">
      <span className="inline-flex items-center gap-1">
        <HugeiconsIcon icon={Notification01Icon} className="size-3" /> in-app
      </span>
      {sel.email && (
        <span className="inline-flex items-center gap-1">
          <HugeiconsIcon icon={Mail01Icon} className="size-3" /> email
        </span>
      )}
      {sel.webhookIds.length > 0 && (
        <span className="inline-flex items-center gap-1">
          <HugeiconsIcon icon={WebhookIcon} className="size-3" />
          {sel.webhookIds.length} webhook
          {sel.webhookIds.length > 1 ? "s" : ""}
        </span>
      )}
    </span>
  )
}

function RulesSection({
  rules,
  loading,
  templates,
  templatesLoading,
  webhooks,
  alertEmail,
}: {
  rules: AlertRule[]
  loading: boolean
  templates: AlertTemplateInfo[]
  templatesLoading: boolean
  webhooks: MerchantWebhook[]
  alertEmail: string
}) {
  const canCreate = templates.length > 0

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid max-w-2xl gap-1">
          <h2 className="text-base font-semibold">Alert rules</h2>
          <p className="text-sm text-pretty text-muted-foreground">
            Monitor billing risks and notify your team when thresholds are
            crossed.
          </p>
        </div>
        {canCreate ? (
          <RuleDialog
            templates={templates}
            webhooks={webhooks}
            alertEmail={alertEmail}
          />
        ) : (
          <Button size="sm" disabled>
            <HugeiconsIcon icon={Add01Icon} className="size-4" />
            {templatesLoading ? "Loading…" : "New rule"}
          </Button>
        )}
      </div>
      {loading || templatesLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : rules.length === 0 ? (
        <p className="py-2 text-sm text-muted-foreground">
          No alert rules configured.
        </p>
      ) : (
        <Table className="min-w-[48rem]">
          <TableHeader>
            <TableRow>
              <TableHead className="text-muted-foreground">Rule</TableHead>
              <TableHead className="text-muted-foreground">Severity</TableHead>
              <TableHead className="text-muted-foreground">Channels</TableHead>
              <TableHead className="text-muted-foreground">State</TableHead>
              <TableHead className="text-muted-foreground">Enabled</TableHead>
              <TableHead className="text-right text-muted-foreground">
                Action
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rules.map((r) => (
              <RuleRow
                key={r.id}
                rule={r}
                templates={templates}
                webhooks={webhooks}
                alertEmail={alertEmail}
              />
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}

function RuleRow({
  rule,
  templates,
  webhooks,
  alertEmail,
}: {
  rule: AlertRule
  templates: AlertTemplateInfo[]
  webhooks: MerchantWebhook[]
  alertEmail: string
}) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const updateRule = useMutation(adminMutations.updateAlertRule(queryClient))
  const deleteRule = useMutation(adminMutations.deleteAlertRule(queryClient))
  const testRule = useMutation(adminMutations.testAlertRule())
  const def = templateByKey(templates, rule.template)
  const label = rule.name || (def ? templateLabel(def) : rule.template)
  const firing = ruleIsFiring(rule)

  const runUpdate = async (enabled: boolean) => {
    try {
      await updateRule.mutateAsync({ id: rule.id, rule: { enabled } })
      toast.success(enabled ? "Rule enabled" : "Rule disabled")
    } catch (err) {
      toastApiError(err, "Update rule")
    }
  }

  const runTest = async () => {
    try {
      const { results } = await testRule.mutateAsync(rule.id)
      toast.success(summarizeDeliveryResults(results))
    } catch (err) {
      toastApiError(err, "Send test alert")
    }
  }

  const busy =
    updateRule.isPending || deleteRule.isPending || testRule.isPending

  return (
    <TableRow className={rule.enabled ? undefined : "opacity-60"}>
      <TableCell className="py-3">
        <span className="font-medium">{label}</span>
        {def?.digest && (
          <span className="block text-xs text-muted-foreground">
            Sent on a schedule, not when a number crosses a line
          </span>
        )}
      </TableCell>
      <TableCell className="py-3">
        <SeverityBadge severity={rule.severity} />
      </TableCell>
      <TableCell className="py-3">
        <ChannelSummary channels={rule.channels} />
      </TableCell>
      <TableCell className="py-3">
        {firing ? (
          <Badge variant="secondary" className="bg-failed-surface text-failed">
            firing
          </Badge>
        ) : (
          <span className="text-xs text-muted-foreground">ok</span>
        )}
      </TableCell>
      <TableCell className="py-3">
        <Switch
          checked={rule.enabled}
          disabled={busy}
          aria-label={rule.enabled ? "Disable rule" : "Enable rule"}
          onCheckedChange={(next) => void runUpdate(next)}
        />
      </TableCell>
      <TableCell className="py-3 text-right">
        <div className="flex justify-end gap-1">
          <Button variant="ghost" size="sm" disabled={busy} onClick={runTest}>
            <HugeiconsIcon icon={SentIcon} className="size-3.5" /> Test
          </Button>
          <RuleDialog
            rule={rule}
            templates={templates}
            webhooks={webhooks}
            alertEmail={alertEmail}
            trigger={
              <Button variant="outline" size="sm">
                Edit
              </Button>
            }
          />
          <Button
            variant="ghost"
            size="icon"
            aria-label="Delete rule"
            disabled={busy}
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
            title={`Delete "${label}"?`}
            description="This alert rule will stop evaluating and its firing state is discarded. This cannot be undone."
            confirmationWord="DELETE"
            actionLabel="Delete rule"
            onConfirm={async () => {
              try {
                await deleteRule.mutateAsync(rule.id)
                toast.success("Rule deleted")
              } catch (err) {
                toastApiError(err, "Delete rule")
              }
            }}
          />
        </div>
      </TableCell>
    </TableRow>
  )
}

// --- Rule create/edit dialog ----------------------------------------------

interface RuleFormValues {
  template: AlertTemplate
  severity: AlertSeverity
  params: Record<string, string>
  channels: ChannelSelection
  enabled: boolean
}

function ruleFormValues(
  templates: AlertTemplateInfo[],
  template: AlertTemplate,
  rule?: AlertRule
): RuleFormValues {
  const definition = templateByKey(templates, template)
  const params: Record<string, string> = {}
  for (const spec of definition?.params ?? []) {
    const existing = rule?.params?.[spec.name]
    if (existing !== undefined && existing !== null) {
      params[spec.name] = String(existing)
    } else if (spec.default !== undefined) {
      params[spec.name] = String(spec.default)
    } else {
      params[spec.name] = ""
    }
  }

  const severity = rule?.severity ?? definition?.default_severity ?? "warning"
  return {
    template,
    severity,
    params,
    channels: rule
      ? channelsFromWire(rule.channels)
      : { in_app: true, email: severity === "critical", webhookIds: [] },
    enabled: rule?.enabled ?? true,
  }
}

function RuleDialog({
  rule,
  templates,
  webhooks,
  alertEmail,
  trigger,
}: {
  rule?: AlertRule
  templates: AlertTemplateInfo[]
  webhooks: MerchantWebhook[]
  alertEmail: string
  trigger?: React.ReactElement
}) {
  const editing = !!rule
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const createRule = useMutation(adminMutations.createAlertRule(queryClient))
  const updateRule = useMutation(adminMutations.updateAlertRule(queryClient))
  const initialTemplate = rule?.template ?? templates[0]?.key ?? ""
  const form = useForm({
    defaultValues: ruleFormValues(templates, initialTemplate, rule),
    onSubmit: async ({ value }) => {
      const definition = templateByKey(templates, value.template)
      if (!definition) return
      const params: Record<string, unknown> = {}
      for (const spec of definition.params) {
        const raw = (value.params[spec.name] ?? "").trim()
        if (raw === "") continue
        params[spec.name] = spec.type === "window" ? raw : Number(raw)
      }
      const request: AlertRuleRequest = {
        template: value.template,
        params,
        severity: value.severity,
        channels: channelsToWire(value.channels),
        enabled: value.enabled,
      }
      try {
        if (rule) {
          await updateRule.mutateAsync({ id: rule.id, rule: request })
        } else {
          await createRule.mutateAsync(request)
        }
        toast.success(editing ? "Rule updated" : "Rule created")
        setOpen(false)
      } catch (err) {
        toastApiError(err, editing ? "Update rule" : "Create rule")
      }
    },
  })

  const handleOpen = (next: boolean) => {
    setOpen(next)
    if (next) {
      form.reset(ruleFormValues(templates, initialTemplate, rule))
    }
  }

  const pickTemplate = (tpl: AlertTemplate) => {
    form.reset(ruleFormValues(templates, tpl))
  }

  return (
    <Dialog open={open} onOpenChange={handleOpen}>
      <DialogTrigger
        render={
          trigger ?? (
            <Button size="sm">
              <HugeiconsIcon icon={Add01Icon} className="size-4" /> New rule
            </Button>
          )
        }
      />
      <DialogContent
        className={cn("max-h-[85vh] overflow-y-auto", DIALOG_FORM)}
      >
        <form.Subscribe selector={(state) => state.values}>
          {(values) => {
            const definition = templateByKey(templates, values.template)
            const missingRequired = (definition?.params ?? []).some(
              (spec) => spec.required && !values.params[spec.name]?.trim()
            )

            return (
              <>
                <DialogHeader>
                  <DialogTitle>
                    {editing ? "Edit alert rule" : "New alert rule"}
                  </DialogTitle>
                  <DialogDescription>
                    {editing && definition
                      ? templateDescription(definition)
                      : "Pick what to watch, set the threshold, and choose how you want to be told."}
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
                  {!editing && (
                    <div className="grid gap-2">
                      <Label>What to watch</Label>
                      <div className="grid gap-2">
                        {templates.map((template) => (
                          <Button
                            key={template.key}
                            type="button"
                            variant="outline"
                            onClick={() => pickTemplate(template.key)}
                            aria-pressed={values.template === template.key}
                            className={cn(
                              "h-auto flex-col items-start gap-1 p-3 text-left whitespace-normal",
                              values.template === template.key &&
                                "border-primary bg-primary/5"
                            )}
                          >
                            <span className="text-sm font-medium">
                              {templateLabel(template)}
                            </span>
                            <span className="text-xs text-muted-foreground">
                              {templateDescription(template)}
                            </span>
                          </Button>
                        ))}
                      </div>
                    </div>
                  )}

                  <form.Field name="params">
                    {(field) => (
                      <div className="grid gap-3">
                        {(definition?.params ?? []).map((spec) => {
                          const readOnly = isFixedParam(
                            values.template,
                            spec.name
                          )
                          return (
                            <div key={spec.name} className="grid gap-1.5">
                              <Label htmlFor={`p-${spec.name}`}>
                                {humanizeParam(spec.name)}
                              </Label>
                              <Input
                                id={`p-${spec.name}`}
                                type={
                                  spec.type === "window" ? "text" : "number"
                                }
                                step={
                                  spec.type === "integer"
                                    ? 1
                                    : spec.type === "number"
                                      ? "any"
                                      : undefined
                                }
                                min={spec.min}
                                max={spec.max}
                                placeholder={
                                  spec.type === "window"
                                    ? "e.g. 30d, 12w"
                                    : undefined
                                }
                                disabled={readOnly}
                                value={field.state.value[spec.name] ?? ""}
                                onChange={(event) =>
                                  field.handleChange({
                                    ...field.state.value,
                                    [spec.name]: event.target.value,
                                  })
                                }
                              />
                              <p className="text-xs text-muted-foreground">
                                {paramDescription(spec.name, spec.description)}
                              </p>
                            </div>
                          )
                        })}
                      </div>
                    )}
                  </form.Field>

                  <form.Field name="severity">
                    {(field) => (
                      <div className="grid gap-1.5">
                        <Label>Severity</Label>
                        <Select
                          items={SEVERITY_ITEMS}
                          value={field.state.value}
                          onValueChange={(value) =>
                            field.handleChange(value as AlertSeverity)
                          }
                        >
                          <SelectTrigger className="w-full">
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            {SEVERITY_ITEMS.map((item) => (
                              <SelectItem key={item.value} value={item.value}>
                                {item.label}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      </div>
                    )}
                  </form.Field>

                  <form.Field name="channels">
                    {(field) => (
                      <div className="grid gap-2">
                        <Label>Where to send it</Label>
                        <div className="grid gap-2 rounded-md border p-3">
                          <div className="flex items-center justify-between">
                            <span className="inline-flex items-center gap-2 text-sm">
                              <HugeiconsIcon
                                icon={Notification01Icon}
                                className="size-4"
                              />
                              In the console
                            </span>
                            <Badge variant="secondary" className="text-[10px]">
                              always on
                            </Badge>
                          </div>

                          <div className="grid gap-1.5">
                            <div className="flex items-center justify-between">
                              <span className="inline-flex items-center gap-2 text-sm">
                                <HugeiconsIcon
                                  icon={Mail01Icon}
                                  className="size-4"
                                />
                                Email
                              </span>
                              <Switch
                                checked={field.state.value.email}
                                onCheckedChange={(email) =>
                                  field.handleChange({
                                    ...field.state.value,
                                    email,
                                  })
                                }
                                aria-label="Toggle email channel"
                              />
                            </div>
                            {field.state.value.email &&
                              (alertEmail ? (
                                <p className="text-xs text-muted-foreground">
                                  Sent to{" "}
                                  <span className="font-medium text-foreground">
                                    {alertEmail}
                                  </span>
                                  .
                                </p>
                              ) : (
                                <p className="text-xs text-held">
                                  No alert email set, so nothing will be emailed
                                  until you add one above. The alert still
                                  reaches the console and any webhooks.
                                </p>
                              ))}
                          </div>

                          <div className="grid gap-1.5">
                            <span className="inline-flex items-center gap-2 text-sm">
                              <HugeiconsIcon
                                icon={WebhookIcon}
                                className="size-4"
                              />
                              Webhooks
                            </span>
                            {webhooks.length === 0 ? (
                              <p className="text-xs text-muted-foreground">
                                No webhooks yet. Add one below to send alerts to
                                your own systems.
                              </p>
                            ) : (
                              <div className="grid gap-1">
                                {webhooks.map((webhook) => {
                                  const selected =
                                    field.state.value.webhookIds.includes(
                                      webhook.id
                                    )
                                  return (
                                    <label
                                      key={webhook.id}
                                      className="flex cursor-pointer items-center justify-between gap-2 rounded-md px-2 py-1 text-sm hover:bg-muted/50"
                                    >
                                      <span className="inline-flex items-center gap-2">
                                        <span>{webhook.name}</span>
                                        <Badge
                                          variant="secondary"
                                          className="text-[10px]"
                                        >
                                          {webhook.format}
                                        </Badge>
                                      </span>
                                      <Switch
                                        checked={selected}
                                        aria-label={`Toggle webhook ${webhook.name}`}
                                        onCheckedChange={(next) =>
                                          field.handleChange({
                                            ...field.state.value,
                                            webhookIds: next
                                              ? [
                                                  ...field.state.value
                                                    .webhookIds,
                                                  webhook.id,
                                                ]
                                              : field.state.value.webhookIds.filter(
                                                  (id) => id !== webhook.id
                                                ),
                                          })
                                        }
                                      />
                                    </label>
                                  )
                                })}
                              </div>
                            )}
                          </div>
                        </div>
                      </div>
                    )}
                  </form.Field>

                  <DialogFooter>
                    <Button
                      type="button"
                      variant="outline"
                      onClick={() => handleOpen(false)}
                    >
                      Cancel
                    </Button>
                    <form.Subscribe selector={(state) => state.isSubmitting}>
                      {(isSubmitting) => (
                        <Button
                          type="submit"
                          disabled={
                            isSubmitting || !definition || missingRequired
                          }
                        >
                          {isSubmitting
                            ? "Saving…"
                            : editing
                              ? "Save changes"
                              : "Create rule"}
                        </Button>
                      )}
                    </form.Subscribe>
                  </DialogFooter>
                </form>
              </>
            )
          }}
        </form.Subscribe>
      </DialogContent>
    </Dialog>
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
            <TableRow>
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
        title={webhook.url}
      >
        {webhook.url}
      </TableCell>
      <TableCell className="py-3 text-right">
        <div className="flex justify-end gap-1">
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
            description="Any alert rule routing to this webhook will stop delivering to it. This cannot be undone."
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

function WebhookDialog() {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const addWebhook = useMutation(adminMutations.createWebhook(queryClient))
  const form = useForm({
    defaultValues: {
      name: "",
      url: "",
      format: "generic" as WebhookFormat,
    },
    onSubmit: async ({ value }) => {
      try {
        await addWebhook.mutateAsync({
          name: value.name.trim(),
          url: value.url.trim(),
          format: value.format,
        })
        toast.success("Webhook added")
        handleOpen(false)
      } catch (err) {
        toastApiError(err, "Add webhook")
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
            <HugeiconsIcon icon={Add01Icon} className="size-4" /> Add webhook
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>Add webhook</DialogTitle>
          <DialogDescription>
            Paste the webhook address from a Discord or Slack channel, no bot
            needed, or any address of your own that can receive alerts.
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
                  value.trim() ? undefined : "Enter a webhook name",
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
                    !name.trim() || !url.trim() || !canSubmit || isSubmitting
                  }
                >
                  {isSubmitting ? "Adding…" : "Add webhook"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
