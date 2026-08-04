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
import { useApiData } from "@/hooks/use-api-data"
import {
  createAlertRule,
  createWebhook,
  deleteAlertRule,
  deleteWebhook,
  getMerchantSettings,
  listAlertRules,
  listAlertTemplates,
  listWebhooks,
  putMerchantSettings,
  testAlertRule,
  updateAlertRule,
  type AlertRuleRequest,
} from "@/lib/api/endpoints"
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
      "Warns before card-network monitoring thresholds. Fires per rail account when its " +
      "chargeback ratio approaches the network limit (VAMP ≈ 0.9%) — the alert that gets a " +
      "high-risk merchant fined.",
  },
  dunning_spike: {
    label: "Dunning spike",
    description:
      "Fires when the number of subscriptions in dunning (past_due) jumps versus its trailing " +
      "baseline — an early signal that a rail is suddenly declining hard.",
  },
  payers_at_depletion_risk: {
    label: "Payers approaching credit depletion",
    description:
      "Fires when a batch of prepaid payers are about to run out of credits — the top-up revenue " +
      "moment, and a churn-risk signal for usage-billing platforms.",
  },
  payment_methods_expiring: {
    label: "Payment methods expiring (monthly digest)",
    description:
      "A monthly heads-up of cards expiring soon so you can prompt updates before involuntary " +
      "churn hits.",
  },
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
    .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : w))
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
  const detail = failed.map((f) => f.detail || f.channel).join("; ")
  return `Test alert: ${results.length - failed.length}/${results.length} channels succeeded (${detail})`
}

// --- Page ------------------------------------------------------------------

export function AlertsTab() {
  const settings = useApiData(() => getMerchantSettings(), [])
  const templates = useApiData(() => listAlertTemplates(), [])
  const rules = useApiData(() => listAlertRules(), [])
  const webhooks = useApiData(() => listWebhooks(), [])

  React.useEffect(() => {
    if (settings.error) toastApiError(settings.error, "Load settings")
  }, [settings.error])
  React.useEffect(() => {
    if (templates.error) toastApiError(templates.error, "Load alert templates")
  }, [templates.error])
  React.useEffect(() => {
    if (rules.error) toastApiError(rules.error, "Load alert rules")
  }, [rules.error])
  React.useEffect(() => {
    if (webhooks.error) toastApiError(webhooks.error, "Load webhooks")
  }, [webhooks.error])

  const alertEmail = settings.data?.alert_email ?? ""
  const hooks = webhooks.data?.data ?? []
  const templateList = templates.data?.data ?? []

  return (
    <div className="flex flex-col gap-10">
      <AlertEmailSection
        key={settings.data?.alert_email ?? "∅"}
        settings={settings.data ?? undefined}
        loading={settings.loading}
        onSaved={settings.reload}
      />
      <RulesSection
        rules={rules.data?.data ?? []}
        loading={rules.loading}
        templates={templateList}
        templatesLoading={templates.loading}
        webhooks={hooks}
        alertEmail={alertEmail}
        onChanged={rules.reload}
      />
      <WebhooksSection
        webhooks={hooks}
        loading={webhooks.loading}
        onChanged={webhooks.reload}
      />
    </div>
  )
}

// --- Alert email -----------------------------------------------------------

function AlertEmailSection({
  settings,
  loading,
  onSaved,
}: {
  settings?: MerchantSettings
  loading: boolean
  onSaved: () => void
}) {
  // Initial value is seeded from props; the parent remounts this section (key on
  // alert_email) when the saved value changes, so no props→state effect.
  const initial = settings?.alert_email ?? ""
  const [email, setEmail] = React.useState(initial)
  const [busy, setBusy] = React.useState(false)

  const dirty = email.trim() !== initial

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">Email delivery</h2>
          <p className="text-sm text-muted-foreground">
            Send email alerts to this address.
          </p>
        </div>
        <Button
          size="sm"
          disabled={busy || loading || !dirty}
          onClick={async () => {
            setBusy(true)
            try {
              // Preserve existing settings; only change alert_email.
              await putMerchantSettings({
                ...(settings ?? {}),
                alert_email: email.trim() || undefined,
              })
              toast.success(
                email.trim() ? "Alert email saved" : "Alert email cleared"
              )
              onSaved()
            } catch (err) {
              toastApiError(err, "Save alert email")
            } finally {
              setBusy(false)
            }
          }}
        >
          {busy ? "Saving…" : "Save"}
        </Button>
      </div>
      <div className="grid gap-2 md:grid-cols-[11rem_minmax(0,1fr)] md:items-center md:gap-6">
        <Label htmlFor="alert-email">Alert email</Label>
        <div className="min-w-0">
          <Input
            id="alert-email"
            type="email"
            placeholder="alerts@example.com"
            value={email}
            disabled={loading}
            onChange={(e) => setEmail(e.target.value)}
          />
        </div>
      </div>
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
          ? "bg-red-500/15 text-red-600 dark:text-red-400"
          : "bg-amber-500/15 text-amber-600 dark:text-amber-400"
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
          <HugeiconsIcon icon={WebhookIcon} className="size-3" />{" "}
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
  onChanged,
}: {
  rules: AlertRule[]
  loading: boolean
  templates: AlertTemplateInfo[]
  templatesLoading: boolean
  webhooks: MerchantWebhook[]
  alertEmail: string
  onChanged: () => void
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
            onDone={onChanged}
          />
        ) : (
          <Button size="sm" disabled>
            <HugeiconsIcon icon={Add01Icon} className="size-4" />{" "}
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
                onChanged={onChanged}
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
  onChanged,
}: {
  rule: AlertRule
  templates: AlertTemplateInfo[]
  webhooks: MerchantWebhook[]
  alertEmail: string
  onChanged: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const def = templateByKey(templates, rule.template)
  const label = rule.name || (def ? templateLabel(def) : rule.template)
  const firing = ruleIsFiring(rule)

  const run = async (
    fn: () => Promise<unknown>,
    action: string,
    ok: string
  ) => {
    setBusy(true)
    try {
      await fn()
      toast.success(ok)
      onChanged()
    } catch (err) {
      toastApiError(err, action)
    } finally {
      setBusy(false)
    }
  }

  const runTest = async () => {
    setBusy(true)
    try {
      const { results } = await testAlertRule(rule.id)
      toast.success(summarizeDeliveryResults(results))
    } catch (err) {
      toastApiError(err, "Send test alert")
    } finally {
      setBusy(false)
    }
  }

  return (
    <TableRow className={rule.enabled ? undefined : "opacity-60"}>
      <TableCell className="py-3">
        <span className="font-medium">{label}</span>
        {def?.digest && (
          <span className="block text-xs text-muted-foreground">
            Digest — periodic, not threshold-triggered
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
          <Badge
            variant="secondary"
            className="bg-red-500/15 text-red-600 dark:text-red-400"
          >
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
          onCheckedChange={(next) =>
            run(
              () => updateAlertRule(rule.id, { enabled: next }),
              "Update rule",
              next ? "Rule enabled" : "Rule disabled"
            )
          }
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
            onDone={onChanged}
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
            onConfirm={() =>
              run(() => deleteAlertRule(rule.id), "Delete rule", "Rule deleted")
            }
          />
        </div>
      </TableCell>
    </TableRow>
  )
}

// --- Rule create/edit dialog ----------------------------------------------

function RuleDialog({
  rule,
  templates,
  webhooks,
  alertEmail,
  onDone,
  trigger,
}: {
  rule?: AlertRule
  templates: AlertTemplateInfo[]
  webhooks: MerchantWebhook[]
  alertEmail: string
  onDone: () => void
  trigger?: React.ReactElement
}) {
  const editing = !!rule
  const [open, setOpen] = React.useState(false)
  const [template, setTemplate] = React.useState<AlertTemplate>(
    rule?.template ?? templates[0]?.key ?? ""
  )
  const [severity, setSeverity] = React.useState<AlertSeverity>(
    rule?.severity ?? "warning"
  )
  const [params, setParams] = React.useState<Record<string, string>>({})
  const [channels, setChannels] = React.useState<ChannelSelection>({
    in_app: true,
    email: false,
    webhookIds: [],
  })
  const [enabled, setEnabled] = React.useState(rule?.enabled ?? true)
  const [busy, setBusy] = React.useState(false)

  const def = templateByKey(templates, template)

  // (Re)initialize form state whenever the dialog opens or the template changes.
  const initFor = React.useCallback(
    (tpl: AlertTemplate, r?: AlertRule) => {
      const d = templateByKey(templates, tpl)
      const p: Record<string, string> = {}
      for (const spec of d?.params ?? []) {
        const existing = r?.params?.[spec.name]
        if (existing !== undefined && existing !== null)
          p[spec.name] = String(existing)
        else if (spec.default !== undefined) p[spec.name] = String(spec.default)
        else p[spec.name] = ""
      }
      setParams(p)
      if (r) {
        setSeverity(r.severity)
        setChannels(channelsFromWire(r.channels))
        setEnabled(r.enabled)
      } else {
        const sev = d?.default_severity ?? "warning"
        setSeverity(sev)
        setChannels({ in_app: true, email: sev === "critical", webhookIds: [] })
        setEnabled(true)
      }
    },
    [templates]
  )

  const handleOpen = (next: boolean) => {
    setOpen(next)
    if (next) {
      const tpl = rule?.template ?? template
      setTemplate(tpl)
      initFor(tpl, rule)
    }
  }

  const pickTemplate = (tpl: AlertTemplate) => {
    setTemplate(tpl)
    initFor(tpl, undefined)
  }

  const missingRequired = (def?.params ?? []).some(
    (spec) => spec.required && !params[spec.name]?.trim()
  )

  const submit = async () => {
    if (!def) return
    const p: Record<string, unknown> = {}
    for (const spec of def.params) {
      const raw = (params[spec.name] ?? "").trim()
      if (raw === "") continue // omit — the server fills the default or requires it
      p[spec.name] = spec.type === "window" ? raw : Number(raw)
    }
    const body: AlertRuleRequest = {
      template,
      params: p,
      severity,
      channels: channelsToWire(channels),
      enabled,
    }
    setBusy(true)
    try {
      if (editing) await updateAlertRule(rule.id, body)
      else await createAlertRule(body)
      toast.success(editing ? "Rule updated" : "Rule created")
      setOpen(false)
      onDone()
    } catch (err) {
      toastApiError(err, editing ? "Update rule" : "Create rule")
    } finally {
      setBusy(false)
    }
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
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {editing ? "Edit alert rule" : "New alert rule"}
          </DialogTitle>
          <DialogDescription>
            {editing && def
              ? templateDescription(def)
              : "Pick what to watch, set the threshold, and choose how you want to be told."}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          {!editing && (
            <div className="grid gap-2">
              <Label>Template</Label>
              <div className="grid gap-2">
                {templates.map((t) => (
                  <button
                    key={t.key}
                    type="button"
                    onClick={() => pickTemplate(t.key)}
                    aria-pressed={template === t.key}
                    className={cn(
                      "rounded-md border p-3 text-left transition-colors",
                      template === t.key
                        ? "border-primary bg-primary/5"
                        : "hover:bg-muted/50"
                    )}
                  >
                    <p className="text-sm font-medium">{templateLabel(t)}</p>
                    <p className="mt-1 text-xs text-muted-foreground">
                      {templateDescription(t)}
                    </p>
                  </button>
                ))}
              </div>
            </div>
          )}

          <div className="grid gap-3">
            {(def?.params ?? []).map((spec) => {
              const readOnly = isFixedParam(template, spec.name)
              return (
                <div key={spec.name} className="grid gap-1.5">
                  <Label htmlFor={`p-${spec.name}`}>
                    {humanizeParam(spec.name)}
                  </Label>
                  <Input
                    id={`p-${spec.name}`}
                    type={spec.type === "window" ? "text" : "number"}
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
                      spec.type === "window" ? "e.g. 30d, 12w" : undefined
                    }
                    disabled={readOnly}
                    value={params[spec.name] ?? ""}
                    onChange={(e) =>
                      setParams((prev) => ({
                        ...prev,
                        [spec.name]: e.target.value,
                      }))
                    }
                  />
                  <p className="text-xs text-muted-foreground">
                    {spec.description}
                  </p>
                </div>
              )
            })}
          </div>

          <div className="grid gap-1.5">
            <Label>Severity</Label>
            <Select
              value={severity}
              onValueChange={(v) => setSeverity(v as AlertSeverity)}
            >
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="warning">
                  Warning — in-app by default
                </SelectItem>
                <SelectItem value="critical">
                  Critical — push harder (email + webhooks)
                </SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="grid gap-2">
            <Label>Channels</Label>
            <div className="grid gap-2 rounded-md border p-3">
              <div className="flex items-center justify-between">
                <span className="inline-flex items-center gap-2 text-sm">
                  <HugeiconsIcon icon={Notification01Icon} className="size-4" />{" "}
                  In-app
                </span>
                <Badge variant="secondary" className="text-[10px]">
                  always on
                </Badge>
              </div>

              <div className="grid gap-1.5">
                <div className="flex items-center justify-between">
                  <span className="inline-flex items-center gap-2 text-sm">
                    <HugeiconsIcon icon={Mail01Icon} className="size-4" /> Email
                  </span>
                  <Switch
                    checked={channels.email}
                    onCheckedChange={(next) =>
                      setChannels((c) => ({ ...c, email: next }))
                    }
                    aria-label="Toggle email channel"
                  />
                </div>
                {channels.email &&
                  (alertEmail ? (
                    <p className="text-xs text-muted-foreground">
                      Sent to{" "}
                      <span className="font-medium text-foreground">
                        {alertEmail}
                      </span>
                      .
                    </p>
                  ) : (
                    <p className="text-xs text-amber-600 dark:text-amber-400">
                      No alert email set — email won&apos;t send until you set
                      one in “Alert email” above (the alert still reaches in-app
                      and webhooks).
                    </p>
                  ))}
              </div>

              <div className="grid gap-1.5">
                <span className="inline-flex items-center gap-2 text-sm">
                  <HugeiconsIcon icon={WebhookIcon} className="size-4" />{" "}
                  Webhooks
                </span>
                {webhooks.length === 0 ? (
                  <p className="text-xs text-muted-foreground">
                    No webhooks configured — add one in the Webhooks section
                    below.
                  </p>
                ) : (
                  <div className="grid gap-1">
                    {webhooks.map((w) => {
                      const on = channels.webhookIds.includes(w.id)
                      return (
                        <label
                          key={w.id}
                          className="flex cursor-pointer items-center justify-between gap-2 rounded-md px-2 py-1 text-sm hover:bg-muted/50"
                        >
                          <span className="inline-flex items-center gap-2">
                            <span>{w.name}</span>
                            <Badge variant="secondary" className="text-[10px]">
                              {w.format}
                            </Badge>
                          </span>
                          <Switch
                            checked={on}
                            aria-label={`Toggle webhook ${w.name}`}
                            onCheckedChange={(next) =>
                              setChannels((c) => ({
                                ...c,
                                webhookIds: next
                                  ? [...c.webhookIds, w.id]
                                  : c.webhookIds.filter((id) => id !== w.id),
                              }))
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
        </div>

        <DialogFooter>
          <Button disabled={busy || !def || missingRequired} onClick={submit}>
            {busy ? "Saving…" : editing ? "Save changes" : "Create rule"}
          </Button>
        </DialogFooter>
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
  onChanged,
}: {
  webhooks: MerchantWebhook[]
  loading: boolean
  onChanged: () => void
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
        <WebhookDialog onDone={onChanged} />
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
              <WebhookRow key={w.id} webhook={w} onChanged={onChanged} />
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}

function WebhookRow({
  webhook,
  onChanged,
}: {
  webhook: MerchantWebhook
  onChanged: () => void
}) {
  const [confirmOpen, setConfirmOpen] = React.useState(false)
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
                await deleteWebhook(webhook.id)
                toast.success("Webhook deleted")
                onChanged()
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

function WebhookDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [name, setName] = React.useState("")
  const [url, setUrl] = React.useState("")
  const [format, setFormat] = React.useState<WebhookFormat>("generic")
  const [busy, setBusy] = React.useState(false)

  const handleOpen = (next: boolean) => {
    setOpen(next)
    if (!next) {
      setName("")
      setUrl("")
      setFormat("generic")
    }
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
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add webhook</DialogTitle>
          <DialogDescription>
            Paste a Discord or Slack channel webhook URL — no bot needed — or a
            “generic” URL your own receiver consumes.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="wh-name">Name</Label>
            <Input
              id="wh-name"
              placeholder="e.g. #billing-alerts"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="wh-format">Format</Label>
            <Select
              value={format}
              onValueChange={(v) => setFormat(v as WebhookFormat)}
            >
              <SelectTrigger id="wh-format" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {WEBHOOK_FORMATS.map((f) => (
                  <SelectItem key={f.value} value={f.value}>
                    {f.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="wh-url">Webhook URL</Label>
            <Input
              id="wh-url"
              className="text-xs"
              placeholder="https://discord.com/api/webhooks/…"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </div>
        </div>
        <DialogFooter>
          <Button
            disabled={busy || !name.trim() || !url.trim()}
            onClick={async () => {
              setBusy(true)
              try {
                await createWebhook({
                  name: name.trim(),
                  url: url.trim(),
                  format,
                })
                toast.success("Webhook added")
                handleOpen(false)
                onDone()
              } catch (err) {
                toastApiError(err, "Add webhook")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Adding…" : "Add webhook"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
