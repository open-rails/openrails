import * as React from "react"
import { BellIcon, MailIcon, PlusIcon, SendIcon, Trash2Icon, WebhookIcon } from "lucide-react"
import { toast } from "sonner"

import { TypedConfirmDialog } from "@/components/typed-confirm-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
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
  listWebhooks,
  putMerchantSettings,
  testAlertRule,
  testWebhook,
  updateAlertRule,
  type AlertRuleRequest,
} from "@/lib/api/endpoints"
import type {
  AlertChannels,
  AlertRule,
  AlertSeverity,
  AlertTemplate,
  MerchantSettings,
  MerchantWebhook,
  WebhookFormat,
} from "@/lib/api/types"
import { cn } from "@/lib/utils"
import { toastApiError } from "@/lib/toast"

// --- Template registry (v1 set, per the #736 contract) --------------------
// Each template maps to a #733 registry query the evaluator runs on the slow
// tick. Params here are the merchant-tunable knobs with the pinned defaults;
// their numeric SEMANTICS are owned by the engine (Agent A) — the UI only
// presents labelled fields and round-trips the values.

type ParamKind = "number" | "duration"

interface ParamSpec {
  key: string
  label: string
  help?: string
  kind: ParamKind
  default: string
  suffix?: string
  step?: string
  min?: string
}

interface TemplateDef {
  key: AlertTemplate
  label: string
  description: string
  defaultSeverity: AlertSeverity
  cadenceNote?: string
  params: ParamSpec[]
}

const TEMPLATES: TemplateDef[] = [
  {
    key: "chargeback_rate",
    label: "Chargeback ratio per merchant account",
    description:
      "Warns before card-network monitoring thresholds. Fires per rail account when its " +
      "chargeback ratio approaches the network limit (VAMP ≈ 0.9%) — the alert that gets a " +
      "high-risk merchant fined.",
    defaultSeverity: "critical",
    params: [
      {
        key: "threshold",
        label: "Warn at fraction of the network limit",
        help: "0.7 = warn at 70% of VAMP (≈ 0.9%), i.e. once chargebacks reach ~0.63%.",
        kind: "number",
        default: "0.7",
        step: "0.05",
        min: "0",
      },
      {
        key: "window",
        label: "Measurement window",
        help: "Trailing window the chargeback ratio is measured over (e.g. 30d).",
        kind: "duration",
        default: "30d",
      },
    ],
  },
  {
    key: "dunning_spike",
    label: "Dunning spike",
    description:
      "Fires when the number of subscriptions in dunning (past_due) jumps versus its trailing " +
      "baseline — an early signal that a rail is suddenly declining hard.",
    defaultSeverity: "critical",
    params: [
      {
        key: "multiplier",
        label: "Spike multiplier",
        help: "Fire when the in-dunning count exceeds this multiple of the baseline (2 = doubled).",
        kind: "number",
        default: "2",
        step: "0.5",
        min: "1",
      },
      {
        key: "window",
        label: "Baseline window",
        help: "Trailing window used to compute the baseline (e.g. 7d).",
        kind: "duration",
        default: "7d",
      },
    ],
  },
  {
    key: "payers_at_depletion_risk",
    label: "Payers approaching credit depletion",
    description:
      "Fires when a batch of prepaid payers are about to run out of credits — the top-up revenue " +
      "moment, and a churn-risk signal for usage-billing platforms.",
    defaultSeverity: "warning",
    params: [
      {
        key: "min_count",
        label: "Minimum payers at risk",
        help: "Only alert when at least this many payers are at risk.",
        kind: "number",
        default: "10",
        min: "1",
      },
      {
        key: "runway_days",
        label: "Runway",
        suffix: "days",
        help: "Count payers whose balance runs out within this many days at their recent burn rate.",
        kind: "number",
        default: "7",
        min: "1",
      },
    ],
  },
  {
    key: "payment_methods_expiring",
    label: "Payment methods expiring (monthly digest)",
    description:
      "A monthly heads-up of cards expiring soon so you can prompt updates before involuntary " +
      "churn hits.",
    defaultSeverity: "warning",
    cadenceNote: "Sent monthly.",
    params: [
      {
        key: "days_ahead",
        label: "Look-ahead",
        suffix: "days",
        help: "Include payment methods expiring within this many days.",
        kind: "number",
        default: "30",
        min: "1",
      },
    ],
  },
]

function templateDef(key: AlertTemplate): TemplateDef {
  return TEMPLATES.find((t) => t.key === key) ?? TEMPLATES[0]
}

// --- Channel wire adapters (localized reconciliation seam) -----------------

interface ChannelSelection {
  in_app: boolean
  email: boolean
  webhookIds: string[]
}

function channelsFromWire(raw: unknown): ChannelSelection {
  if (Array.isArray(raw)) {
    const sel: ChannelSelection = { in_app: false, email: false, webhookIds: [] }
    for (const c of raw) {
      if (c === "in_app") sel.in_app = true
      else if (c === "email") sel.email = true
      else if (typeof c === "string" && c.startsWith("webhook:")) sel.webhookIds.push(c.slice(8))
      else if (c && typeof c === "object") {
        const o = c as Record<string, unknown>
        if (o.type === "in_app") sel.in_app = true
        else if (o.type === "email") sel.email = true
        else if (o.type === "webhook" && typeof o.webhook_id === "string")
          sel.webhookIds.push(o.webhook_id)
      }
    }
    return sel
  }
  const o = (raw ?? {}) as Partial<AlertChannels>
  return {
    in_app: o.in_app ?? true,
    email: o.email ?? false,
    webhookIds: Array.isArray(o.webhook_ids) ? o.webhook_ids : [],
  }
}

// in_app is always on; email/webhooks are the operator's choice.
function channelsToWire(sel: ChannelSelection): AlertChannels {
  return { in_app: true, email: sel.email, webhook_ids: sel.webhookIds }
}

// --- Page ------------------------------------------------------------------

export function AlertsTab() {
  const settings = useApiData(() => getMerchantSettings(), [])
  const rules = useApiData(() => listAlertRules(), [])
  const webhooks = useApiData(() => listWebhooks(), [])

  React.useEffect(() => {
    if (settings.error) toastApiError(settings.error, "Load settings")
  }, [settings.error])
  React.useEffect(() => {
    if (rules.error) toastApiError(rules.error, "Load alert rules")
  }, [rules.error])
  React.useEffect(() => {
    if (webhooks.error) toastApiError(webhooks.error, "Load webhooks")
  }, [webhooks.error])

  const alertEmail = settings.data?.alert_email ?? ""
  const hooks = webhooks.data?.data ?? []

  return (
    <div className="flex max-w-4xl flex-col gap-6">
      <AlertEmailCard
        key={settings.data?.alert_email ?? "∅"}
        settings={settings.data ?? undefined}
        loading={settings.loading}
        onSaved={settings.reload}
      />
      <RulesSection
        rules={rules.data?.data ?? []}
        loading={rules.loading}
        webhooks={hooks}
        alertEmail={alertEmail}
        onChanged={rules.reload}
      />
      <WebhooksSection webhooks={hooks} loading={webhooks.loading} onChanged={webhooks.reload} />
    </div>
  )
}

// --- Alert email -----------------------------------------------------------

function AlertEmailCard({
  settings,
  loading,
  onSaved,
}: {
  settings?: MerchantSettings
  loading: boolean
  onSaved: () => void
}) {
  // Initial value is seeded from props; the parent remounts this card (key on
  // alert_email) when the saved value changes, so no props→state effect.
  const initial = settings?.alert_email ?? ""
  const [email, setEmail] = React.useState(initial)
  const [busy, setBusy] = React.useState(false)

  const dirty = email.trim() !== initial

  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle className="text-sm">Alert email</CardTitle>
        <CardDescription>
          Where operator alerts are sent when a rule uses the email channel. Leave empty and the
          email channel stays inactive — alerts fail soft to in-app and any webhooks.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="grid grid-cols-[1fr_auto] gap-2">
          <Input
            type="email"
            placeholder="alerts@example.com"
            value={email}
            disabled={loading}
            onChange={(e) => setEmail(e.target.value)}
          />
          <Button
            variant="outline"
            disabled={busy || loading || !dirty}
            onClick={async () => {
              setBusy(true)
              try {
                // Preserve existing settings; only change alert_email.
                await putMerchantSettings({ ...(settings ?? {}), alert_email: email.trim() || undefined })
                toast.success(email.trim() ? "Alert email saved" : "Alert email cleared")
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
      </CardContent>
    </Card>
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

function ChannelSummary({ channels }: { channels: AlertChannels }) {
  const sel = channelsFromWire(channels)
  return (
    <span className="flex flex-wrap items-center gap-1 text-xs text-muted-foreground">
      <span className="inline-flex items-center gap-1">
        <BellIcon className="size-3" /> in-app
      </span>
      {sel.email && (
        <span className="inline-flex items-center gap-1">
          <MailIcon className="size-3" /> email
        </span>
      )}
      {sel.webhookIds.length > 0 && (
        <span className="inline-flex items-center gap-1">
          <WebhookIcon className="size-3" /> {sel.webhookIds.length} webhook
          {sel.webhookIds.length > 1 ? "s" : ""}
        </span>
      )}
    </span>
  )
}

function RulesSection({
  rules,
  loading,
  webhooks,
  alertEmail,
  onChanged,
}: {
  rules: AlertRule[]
  loading: boolean
  webhooks: MerchantWebhook[]
  alertEmail: string
  onChanged: () => void
}) {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-sm font-medium">Alert rules</h2>
          <p className="max-w-2xl text-sm text-muted-foreground">
            Threshold alerts run on a slow cadence over your live metrics and push a warning the
            moment a threshold is crossed — before a rail fines you or churn spikes.
          </p>
        </div>
        <RuleDialog webhooks={webhooks} alertEmail={alertEmail} onDone={onChanged} />
      </div>
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : rules.length === 0 ? (
        <Card className="border-dashed">
          <CardContent className="flex flex-col items-center gap-3 py-10 text-center">
            <BellIcon className="size-6 text-muted-foreground" />
            <div>
              <p className="text-sm font-medium">No alert rules yet</p>
              <p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">
                Alerting watches the metrics that can fine or bankrupt you — chargeback ratio,
                dunning spikes, credit depletion, expiring cards — and notifies you when a
                threshold is crossed. Start from a template.
              </p>
            </div>
            <RuleDialog webhooks={webhooks} alertEmail={alertEmail} onDone={onChanged} cta />
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Rule</TableHead>
                <TableHead>Severity</TableHead>
                <TableHead>Channels</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Enabled</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rules.map((r) => (
                <RuleRow
                  key={r.id}
                  rule={r}
                  webhooks={webhooks}
                  alertEmail={alertEmail}
                  onChanged={onChanged}
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

function RuleRow({
  rule,
  webhooks,
  alertEmail,
  onChanged,
}: {
  rule: AlertRule
  webhooks: MerchantWebhook[]
  alertEmail: string
  onChanged: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  const def = templateDef(rule.template)
  const firing = ruleIsFiring(rule)

  const run = async (fn: () => Promise<unknown>, action: string, ok: string) => {
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

  return (
    <TableRow className={rule.enabled ? undefined : "opacity-60"}>
      <TableCell>
        <span className="font-medium">{def.label}</span>
        {def.cadenceNote && (
          <span className="block text-xs text-muted-foreground">{def.cadenceNote}</span>
        )}
      </TableCell>
      <TableCell>
        <SeverityBadge severity={rule.severity} />
      </TableCell>
      <TableCell>
        <ChannelSummary channels={rule.channels} />
      </TableCell>
      <TableCell>
        {firing ? (
          <Badge variant="secondary" className="bg-red-500/15 text-red-600 dark:text-red-400">
            firing
          </Badge>
        ) : (
          <span className="text-xs text-muted-foreground">ok</span>
        )}
      </TableCell>
      <TableCell>
        <Switch
          checked={rule.enabled}
          disabled={busy}
          aria-label={rule.enabled ? "Disable rule" : "Enable rule"}
          onCheckedChange={(next) =>
            run(
              () => updateAlertRule(rule.id, { enabled: next }),
              "Update rule",
              next ? "Rule enabled" : "Rule disabled",
            )
          }
        />
      </TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-1">
          <Button
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={() => run(() => testAlertRule(rule.id), "Send test alert", "Test alert sent")}
          >
            <SendIcon className="size-3.5" /> Test
          </Button>
          <RuleDialog
            rule={rule}
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
            <Trash2Icon className="size-4 text-muted-foreground" />
          </Button>
          <TypedConfirmDialog
            open={confirmOpen}
            onOpenChange={setConfirmOpen}
            title={`Delete "${def.label}"?`}
            description="This alert rule will stop evaluating and its firing state is discarded. This cannot be undone."
            confirmationWord="DELETE"
            actionLabel="Delete rule"
            onConfirm={() => run(() => deleteAlertRule(rule.id), "Delete rule", "Rule deleted")}
          />
        </div>
      </TableCell>
    </TableRow>
  )
}

// --- Rule create/edit dialog ----------------------------------------------

function RuleDialog({
  rule,
  webhooks,
  alertEmail,
  onDone,
  trigger,
  cta,
}: {
  rule?: AlertRule
  webhooks: MerchantWebhook[]
  alertEmail: string
  onDone: () => void
  trigger?: React.ReactNode
  cta?: boolean
}) {
  const editing = !!rule
  const [open, setOpen] = React.useState(false)
  const [template, setTemplate] = React.useState<AlertTemplate>(rule?.template ?? "chargeback_rate")
  const [severity, setSeverity] = React.useState<AlertSeverity>(
    rule?.severity ?? templateDef(template).defaultSeverity,
  )
  const [params, setParams] = React.useState<Record<string, string>>({})
  const [channels, setChannels] = React.useState<ChannelSelection>({
    in_app: true,
    email: false,
    webhookIds: [],
  })
  const [enabled, setEnabled] = React.useState(rule?.enabled ?? true)
  const [busy, setBusy] = React.useState(false)

  const def = templateDef(template)

  // (Re)initialize form state whenever the dialog opens or the template changes.
  const initFor = React.useCallback(
    (tpl: AlertTemplate, r?: AlertRule) => {
      const d = templateDef(tpl)
      const p: Record<string, string> = {}
      for (const spec of d.params) {
        const existing = r?.params?.[spec.key]
        p[spec.key] = existing === undefined || existing === null ? spec.default : String(existing)
      }
      setParams(p)
      if (r) {
        setSeverity(r.severity)
        setChannels(channelsFromWire(r.channels))
        setEnabled(r.enabled)
      } else {
        setSeverity(d.defaultSeverity)
        setChannels({ in_app: true, email: d.defaultSeverity === "critical", webhookIds: [] })
        setEnabled(true)
      }
    },
    [],
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

  const submit = async () => {
    const p: Record<string, unknown> = {}
    for (const spec of def.params) {
      const raw = (params[spec.key] ?? spec.default).trim()
      p[spec.key] = spec.kind === "number" ? Number(raw) : raw
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
      <DialogTrigger asChild>
        {trigger ?? (
          <Button size="sm">
            <PlusIcon className="size-4" /> {cta ? "Create your first rule" : "New rule"}
          </Button>
        )}
      </DialogTrigger>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{editing ? "Edit alert rule" : "New alert rule"}</DialogTitle>
          <DialogDescription>
            {editing
              ? def.description
              : "Pick what to watch, set the threshold, and choose how you want to be told."}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          {!editing && (
            <div className="grid gap-2">
              <Label>Template</Label>
              <div className="grid gap-2">
                {TEMPLATES.map((t) => (
                  <button
                    key={t.key}
                    type="button"
                    onClick={() => pickTemplate(t.key)}
                    aria-pressed={template === t.key}
                    className={cn(
                      "rounded-md border p-3 text-left transition-colors",
                      template === t.key ? "border-primary bg-primary/5" : "hover:bg-muted/50",
                    )}
                  >
                    <p className="text-sm font-medium">{t.label}</p>
                    <p className="mt-1 text-xs text-muted-foreground">{t.description}</p>
                  </button>
                ))}
              </div>
            </div>
          )}

          <div className="grid gap-3">
            {def.params.map((spec) => (
              <div key={spec.key} className="grid gap-1.5">
                <Label htmlFor={`p-${spec.key}`}>
                  {spec.label}
                  {spec.suffix ? ` (${spec.suffix})` : ""}
                </Label>
                <Input
                  id={`p-${spec.key}`}
                  type={spec.kind === "number" ? "number" : "text"}
                  step={spec.step}
                  min={spec.min}
                  value={params[spec.key] ?? ""}
                  onChange={(e) => setParams((prev) => ({ ...prev, [spec.key]: e.target.value }))}
                />
                {spec.help && <p className="text-xs text-muted-foreground">{spec.help}</p>}
              </div>
            ))}
          </div>

          <div className="grid gap-1.5">
            <Label>Severity</Label>
            <Select value={severity} onValueChange={(v) => setSeverity(v as AlertSeverity)}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="warning">Warning — in-app by default</SelectItem>
                <SelectItem value="critical">Critical — push harder (email + webhooks)</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="grid gap-2">
            <Label>Channels</Label>
            <div className="grid gap-2 rounded-md border p-3">
              <div className="flex items-center justify-between">
                <span className="inline-flex items-center gap-2 text-sm">
                  <BellIcon className="size-4" /> In-app
                </span>
                <Badge variant="secondary" className="text-[10px]">
                  always on
                </Badge>
              </div>

              <div className="grid gap-1.5">
                <div className="flex items-center justify-between">
                  <span className="inline-flex items-center gap-2 text-sm">
                    <MailIcon className="size-4" /> Email
                  </span>
                  <Switch
                    checked={channels.email}
                    onCheckedChange={(next) => setChannels((c) => ({ ...c, email: next }))}
                    aria-label="Toggle email channel"
                  />
                </div>
                {channels.email &&
                  (alertEmail ? (
                    <p className="text-xs text-muted-foreground">
                      Sent to <span className="font-medium text-foreground">{alertEmail}</span>.
                    </p>
                  ) : (
                    <p className="text-xs text-amber-600 dark:text-amber-400">
                      No alert email set — email won&apos;t send until you set one in “Alert email”
                      above (the alert still reaches in-app and webhooks).
                    </p>
                  ))}
              </div>

              <div className="grid gap-1.5">
                <span className="inline-flex items-center gap-2 text-sm">
                  <WebhookIcon className="size-4" /> Webhooks
                </span>
                {webhooks.length === 0 ? (
                  <p className="text-xs text-muted-foreground">
                    No webhooks configured — add one in the Webhooks section below.
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
          <Button disabled={busy} onClick={submit}>
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
    <div className="flex flex-col gap-3">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-sm font-medium">Webhooks</h2>
          <p className="max-w-2xl text-sm text-muted-foreground">
            Deliver alerts to a chat channel or your own receiver. Paste a Discord or Slack channel
            webhook URL — no bot needed — or point “generic” at your own endpoint.
          </p>
        </div>
        <WebhookDialog onDone={onChanged} />
      </div>
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : webhooks.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No webhooks yet. Add one to route alerts to Discord, Slack, or a custom URL.
        </p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Format</TableHead>
                <TableHead>URL</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {webhooks.map((w) => (
                <WebhookRow key={w.id} webhook={w} onChanged={onChanged} />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

function WebhookRow({ webhook, onChanged }: { webhook: MerchantWebhook; onChanged: () => void }) {
  const [busy, setBusy] = React.useState(false)
  const [confirmOpen, setConfirmOpen] = React.useState(false)
  return (
    <TableRow className={webhook.enabled === false ? "opacity-60" : undefined}>
      <TableCell className="font-medium">{webhook.name}</TableCell>
      <TableCell>
        <Badge variant="secondary">{webhook.format}</Badge>
      </TableCell>
      <TableCell className="max-w-[22rem] truncate font-mono text-xs" title={webhook.url}>
        {webhook.url}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-1">
          <Button
            variant="ghost"
            size="sm"
            disabled={busy}
            onClick={async () => {
              setBusy(true)
              try {
                await testWebhook(webhook.id)
                toast.success("Test payload sent")
              } catch (err) {
                toastApiError(err, "Send test payload")
              } finally {
                setBusy(false)
              }
            }}
          >
            <SendIcon className="size-3.5" /> Test
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Delete webhook"
            disabled={busy}
            onClick={() => setConfirmOpen(true)}
          >
            <Trash2Icon className="size-4 text-muted-foreground" />
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
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <PlusIcon className="size-4" /> Add webhook
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add webhook</DialogTitle>
          <DialogDescription>
            Paste a Discord or Slack channel webhook URL — no bot needed — or a “generic” URL your
            own receiver consumes.
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
            <Select value={format} onValueChange={(v) => setFormat(v as WebhookFormat)}>
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
              className="font-mono text-xs"
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
                await createWebhook({ name: name.trim(), url: url.trim(), format })
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
