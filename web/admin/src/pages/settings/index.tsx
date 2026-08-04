import * as React from "react"
import { toast } from "sonner"

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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useApiData } from "@/hooks/use-api-data"
import {
  deletePaymentProvider,
  getCreditLimit,
  getMerchantSettings,
  getTrustLevel,
  listPaymentProviders,
  putMerchantSettings,
  putPaymentProvider,
  setCreditLimit,
} from "@/lib/api/endpoints"
import type {
  PaymentProviderConfig,
  PaymentProviderDefinition,
} from "@/lib/api/types"
import { formatDate, formatMicros, microsFromInput } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { AlertsTab } from "./alerts"
import { ApiKeysTab } from "./api-keys"
import { TeamTab } from "./team"

const LINE_TAB =
  "flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"

export function SettingsPage() {
  return (
    <Tabs defaultValue="merchant" className="flex flex-col gap-4">
      <div className="overflow-x-auto">
        <TabsList
          variant="line"
          className="w-max min-w-full justify-start gap-6 rounded-none p-0"
        >
          <TabsTrigger value="merchant" className={LINE_TAB}>
            Merchant
          </TabsTrigger>
          <TabsTrigger value="team" className={LINE_TAB}>
            Team
          </TabsTrigger>
          <TabsTrigger value="alerts" className={LINE_TAB}>
            Alerts
          </TabsTrigger>
          <TabsTrigger value="providers" className={LINE_TAB}>
            Payment providers
          </TabsTrigger>
          <TabsTrigger value="api-keys" className={LINE_TAB}>
            API keys
          </TabsTrigger>
          <TabsTrigger value="customer-controls" className={LINE_TAB}>
            Customer controls
          </TabsTrigger>
        </TabsList>
      </div>
      <TabsContent value="merchant">
        <MerchantSettingsTab />
      </TabsContent>
      <TabsContent value="team">
        <TeamTab />
      </TabsContent>
      <TabsContent value="alerts">
        <AlertsTab />
      </TabsContent>
      <TabsContent value="providers">
        <ProvidersTab />
      </TabsContent>
      <TabsContent value="api-keys">
        <ApiKeysTab />
      </TabsContent>
      <TabsContent value="customer-controls">
        <CustomerControlsTab />
      </TabsContent>
    </Tabs>
  )
}

function MerchantSettingsTab() {
  const { data, loading, error } = useApiData(() => getMerchantSettings(), [])
  React.useEffect(() => {
    if (error) toastApiError(error, "Load settings")
  }, [error])
  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  return (
    <div className="grid max-w-3xl gap-10">
      <MerchantProfileForm initial={data?.profile} />
      <RepriceNoticeWindowForm initial={data?.reprice_notice_window_days} />
    </div>
  )
}

function MerchantProfileForm({
  initial,
}: {
  initial?: {
    display_name?: string
    from_email?: string
    support_url?: string
    logo_url?: string
  }
}) {
  const [displayName, setDisplayName] = React.useState(
    initial?.display_name ?? ""
  )
  const [fromEmail, setFromEmail] = React.useState(initial?.from_email ?? "")
  const [supportURL, setSupportURL] = React.useState(initial?.support_url ?? "")
  const [logoURL, setLogoURL] = React.useState(initial?.logo_url ?? "")
  const [savedProfile, setSavedProfile] = React.useState({
    displayName: initial?.display_name ?? "",
    fromEmail: initial?.from_email ?? "",
    supportURL: initial?.support_url ?? "",
    logoURL: initial?.logo_url ?? "",
  })
  const [busy, setBusy] = React.useState(false)
  const [editing, setEditing] = React.useState(false)

  const reset = () => {
    setDisplayName(savedProfile.displayName)
    setFromEmail(savedProfile.fromEmail)
    setSupportURL(savedProfile.supportURL)
    setLogoURL(savedProfile.logoURL)
  }

  const save = async (event: React.FormEvent) => {
    event.preventDefault()
    setBusy(true)
    try {
      await putMerchantSettings({
        profile: {
          display_name: displayName || undefined,
          from_email: fromEmail || undefined,
          support_url: supportURL || undefined,
          logo_url: logoURL || undefined,
        },
      })
      setSavedProfile({ displayName, fromEmail, supportURL, logoURL })
      toast.success("Profile saved")
      setEditing(false)
    } catch (err) {
      toastApiError(err, "Save profile")
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">Merchant profile</h2>
          <p className="text-sm text-pretty text-muted-foreground">
            Customer-facing merchant details used on invoices and emails.
          </p>
        </div>
        {editing ? (
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => {
                reset()
                setEditing(false)
              }}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              size="sm"
              form="merchant-profile-form"
              disabled={busy}
            >
              {busy ? "Saving…" : "Save"}
            </Button>
          </div>
        ) : (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setEditing(true)}
          >
            Edit
          </Button>
        )}
      </div>

      {editing ? (
        <form id="merchant-profile-form" onSubmit={save} className="grid gap-4">
          <SettingEditField label="Display name" id="s-name">
            <Input
              id="s-name"
              value={displayName}
              onChange={(event) => setDisplayName(event.target.value)}
              autoComplete="organization"
            />
          </SettingEditField>
          <SettingEditField label="From email" id="s-email">
            <Input
              id="s-email"
              type="email"
              value={fromEmail}
              onChange={(event) => setFromEmail(event.target.value)}
              autoComplete="email"
            />
          </SettingEditField>
          <SettingEditField label="Support URL" id="s-support">
            <Input
              id="s-support"
              type="url"
              value={supportURL}
              onChange={(event) => setSupportURL(event.target.value)}
              placeholder="https://example.com/support"
            />
          </SettingEditField>
          <SettingEditField label="Logo URL" id="s-logo">
            <Input
              id="s-logo"
              type="url"
              value={logoURL}
              onChange={(event) => setLogoURL(event.target.value)}
              placeholder="https://example.com/logo.png"
            />
          </SettingEditField>
        </form>
      ) : (
        <dl className="grid gap-4">
          <SettingDetail
            label="Display name"
            value={savedProfile.displayName}
          />
          <SettingDetail label="From email" value={savedProfile.fromEmail} />
          <SettingDetail label="Support URL" value={savedProfile.supportURL} />
          <SettingDetail label="Logo URL" value={savedProfile.logoURL} />
        </dl>
      )}
    </section>
  )
}

// RepriceNoticeWindowForm (#781): the merchant-configurable minimum advance
// notice (days) a subscription price INCREASE must give existing
// subscribers. The catalog price-change wizard reads this same value
// (GET /v1/merchant/settings) for its own date-picker gate; the API enforces
// it regardless of what the console shows.
function RepriceNoticeWindowForm({ initial }: { initial?: number }) {
  const [days, setDays] = React.useState(String(initial ?? 30))
  const [savedDays, setSavedDays] = React.useState(String(initial ?? 30))
  const [busy, setBusy] = React.useState(false)
  const [editing, setEditing] = React.useState(false)
  const parsed = Number(days)
  const valid = days.trim() !== "" && Number.isInteger(parsed) && parsed >= 0

  const save = async (event: React.FormEvent) => {
    event.preventDefault()
    setBusy(true)
    try {
      await putMerchantSettings({
        reprice_notice_window_days: parsed,
      })
      setSavedDays(days)
      toast.success("Notice window saved")
      setEditing(false)
    } catch (err) {
      toastApiError(err, "Save notice window")
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="grid gap-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid max-w-2xl gap-1">
          <h2 className="text-base font-semibold">Pricing policies</h2>
          <p className="text-sm text-pretty text-muted-foreground">
            Set how much notice customers receive before a price increase.
          </p>
        </div>
        {editing ? (
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => {
                setDays(savedDays)
                setEditing(false)
              }}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              size="sm"
              form="notice-window-form"
              disabled={busy || !valid}
            >
              {busy ? "Saving…" : "Save"}
            </Button>
          </div>
        ) : (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setEditing(true)}
          >
            Edit
          </Button>
        )}
      </div>

      {editing ? (
        <form id="notice-window-form" onSubmit={save}>
          <SettingEditField label="Notice period (days)" id="s-notice-window">
            <Input
              id="s-notice-window"
              type="number"
              step="1"
              min="0"
              value={days}
              onChange={(event) => setDays(event.target.value)}
            />
          </SettingEditField>
        </form>
      ) : (
        <dl>
          <SettingDetail label="Notice period" value={`${savedDays} days`} />
        </dl>
      )}
    </section>
  )
}

function ProvidersTab() {
  const { data, loading, error, reload } = useApiData(
    () => listPaymentProviders(),
    []
  )
  React.useEffect(() => {
    if (error) toastApiError(error, "Load payment providers")
  }, [error])

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <ProviderDialog
          providerDefinitions={data?.provider_definitions ?? []}
          onDone={reload}
        />
      </div>
      {!data?.data?.length ? (
        <p className="text-sm text-muted-foreground">
          No payment providers configured.
        </p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Rail</TableHead>
                <TableHead>Environment</TableHead>
                <TableHead>Account</TableHead>
                <TableHead>Credentials</TableHead>
                <TableHead>State</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.data.map((p) => (
                <ProviderRow key={p.id} provider={p} onDone={reload} />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

function ProviderRow({
  provider,
  onDone,
}: {
  provider: PaymentProviderConfig
  onDone: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  return (
    <TableRow className={provider.archived ? "opacity-60" : undefined}>
      <TableCell className="font-medium">{provider.rail}</TableCell>
      <TableCell>{provider.environment}</TableCell>
      <TableCell className="text-xs">{provider.account_id}</TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {Object.entries(provider.credentials).map(([name, c]) => (
            <Badge
              key={name}
              variant="secondary"
              className={
                c.configured
                  ? ""
                  : "bg-amber-500/15 text-amber-600 dark:text-amber-400"
              }
              title={
                c.last_validated_at
                  ? `validated ${formatDate(c.last_validated_at)}`
                  : undefined
              }
            >
              {name}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>
        {provider.archived ? (
          <Badge variant="secondary">archived</Badge>
        ) : provider.drained ? (
          <Badge
            variant="secondary"
            className="bg-amber-500/15 text-amber-600 dark:text-amber-400"
          >
            draining ({provider.open_obligations})
          </Badge>
        ) : (
          <Badge
            variant="secondary"
            className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
          >
            active
          </Badge>
        )}
      </TableCell>
      <TableCell className="text-right">
        {!provider.archived && (
          <Button
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={async () => {
              setBusy(true)
              try {
                await deletePaymentProvider(provider.rail, provider.environment)
                toast.success("Provider archived")
                onDone()
              } catch (err) {
                toastApiError(err, "Archive provider")
              } finally {
                setBusy(false)
              }
            }}
          >
            Archive
          </Button>
        )}
      </TableCell>
    </TableRow>
  )
}

function ProviderDialog({
  providerDefinitions,
  onDone,
}: {
  providerDefinitions: PaymentProviderDefinition[]
  onDone: () => void
}) {
  const [open, setOpen] = React.useState(false)
  const [rail, setRail] = React.useState("")
  const [accountID, setAccountID] = React.useState("")
  const [environment, setEnvironment] = React.useState("")
  const [credentials, setCredentials] = React.useState<Record<string, string>>(
    {}
  )
  const [busy, setBusy] = React.useState(false)
  const selectedProvider = providerDefinitions.find(
    (provider) => provider.rail === rail
  )
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm">Configure provider</Button>} />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Configure payment provider</DialogTitle>
          <DialogDescription>
            account_id is operator-declared per rail (NMI gateway id, Stripe
            acct_…, CCBill clientAccnum-clientSubacc, Solana wallet).
            Credentials are stored in the secret backend.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <Field label="Rail" id="pv-rail">
            <select
              id="pv-rail"
              className="h-9 rounded-md border bg-transparent px-3 text-sm"
              value={rail}
              onChange={(e) => {
                setRail(e.target.value)
                setCredentials({})
              }}
            >
              <option value="">Pick a rail…</option>
              {providerDefinitions.map((provider) => (
                <option key={provider.rail} value={provider.rail}>
                  {provider.rail} — {provider.display_name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Account id" id="pv-acct">
            <Input
              id="pv-acct"
              value={accountID}
              onChange={(e) => setAccountID(e.target.value)}
            />
          </Field>
          <Field
            label="Environment (live | test, empty = deployment default)"
            id="pv-env"
          >
            <Input
              id="pv-env"
              value={environment}
              onChange={(e) => setEnvironment(e.target.value)}
            />
          </Field>
          {selectedProvider?.credential_keys.map((name) => (
            <Field key={name} label={name} id={`pv-credential-${name}`}>
              <Input
                id={`pv-credential-${name}`}
                type="password"
                autoComplete="new-password"
                value={credentials[name] ?? ""}
                onChange={(e) =>
                  setCredentials((current) => ({
                    ...current,
                    [name]: e.target.value,
                  }))
                }
              />
            </Field>
          ))}
        </div>
        <DialogFooter>
          <Button
            disabled={busy || !rail || !accountID.trim()}
            onClick={async () => {
              const creds = Object.fromEntries(
                Object.entries(credentials).filter(([, value]) => value !== "")
              )
              setBusy(true)
              try {
                await putPaymentProvider(rail, {
                  account_id: accountID.trim(),
                  ...(environment ? { environment } : {}),
                  ...(Object.keys(creds).length ? { credentials: creds } : {}),
                })
                setCredentials({})
                toast.success("Provider saved")
                setOpen(false)
                onDone()
              } catch (err) {
                toastApiError(err, "Save provider")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function CustomerControlsTab() {
  const [customerID, setCustomerID] = React.useState("")
  const [currency, setCurrency] = React.useState("usd")
  const [result, setResult] = React.useState<{
    creditLimit: number
    trustLevel: string
  }>()
  const [newLimit, setNewLimit] = React.useState("")
  const [busy, setBusy] = React.useState(false)

  const lookup = async () => {
    setBusy(true)
    try {
      const [cl, tt] = await Promise.all([
        getCreditLimit(customerID.trim(), currency.trim()),
        getTrustLevel(customerID.trim(), currency.trim()),
      ])
      setResult({
        creditLimit: cl.credit_limit_amount,
        trustLevel: tt.trust_level,
      })
    } catch (err) {
      toastApiError(err, "Lookup customer controls")
      setResult(undefined)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle className="text-sm">Credit limit & trust level</CardTitle>
        <CardDescription>
          Per-customer, per-currency: the arrears credit limit is writable; the
          trust level is graduated by spend history (read-only here).
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="grid grid-cols-[1fr_8rem_auto] gap-2">
          <Input
            placeholder="customer id (UUID)"
            value={customerID}
            onChange={(e) => setCustomerID(e.target.value)}
          />
          <Input
            placeholder="currency"
            value={currency}
            onChange={(e) => setCurrency(e.target.value)}
          />
          <Button
            variant="outline"
            disabled={busy || !customerID.trim() || !currency.trim()}
            onClick={lookup}
          >
            Look up
          </Button>
        </div>
        {result && (
          <div className="grid gap-3 rounded-md border p-3 text-sm">
            <p>
              Trust level:{" "}
              <Badge variant="secondary">
                {result.trustLevel || "default"}
              </Badge>
            </p>
            <p>
              Credit limit:{" "}
              {result.creditLimit
                ? formatMicros(result.creditLimit, currency)
                : "off (0)"}
            </p>
            <div className="grid grid-cols-[1fr_auto] gap-2">
              <Input
                placeholder={`new limit in ${currency} (major units, 0 = off)`}
                type="number"
                step="any"
                min="0"
                value={newLimit}
                onChange={(e) => setNewLimit(e.target.value)}
              />
              <Button
                disabled={
                  busy || newLimit === "" || microsFromInput(newLimit) === null
                }
                onClick={async () => {
                  setBusy(true)
                  try {
                    await setCreditLimit(
                      customerID.trim(),
                      currency.trim(),
                      microsFromInput(newLimit) ?? 0
                    )
                    toast.success("Credit limit updated")
                    lookup()
                  } catch (err) {
                    toastApiError(err, "Set credit limit")
                  } finally {
                    setBusy(false)
                  }
                }}
              >
                Set limit
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function Field({
  label,
  id,
  children,
}: {
  label: string
  id: string
  children: React.ReactNode
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}

function SettingDetail({ label, value }: { label: string; value?: string }) {
  return (
    <div className="grid gap-1.5 md:grid-cols-[11rem_minmax(0,1fr)] md:gap-6">
      <dt className="text-sm text-muted-foreground">{label}</dt>
      <dd
        className={
          value
            ? "min-w-0 text-sm break-words"
            : "text-sm text-muted-foreground"
        }
      >
        {value || "Not set"}
      </dd>
    </div>
  )
}

function SettingEditField({
  label,
  id,
  children,
}: {
  label: string
  id: string
  children: React.ReactNode
}) {
  return (
    <div className="grid gap-2 md:grid-cols-[11rem_minmax(0,1fr)] md:items-center md:gap-6">
      <Label htmlFor={id}>{label}</Label>
      <div className="min-w-0">{children}</div>
    </div>
  )
}
