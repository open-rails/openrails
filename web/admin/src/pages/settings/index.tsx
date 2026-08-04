import * as React from "react"
import { toast } from "sonner"

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
    <div className="grid gap-10">
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

  const providerDefinitions = data?.provider_definitions ?? []

  return (
    <div>
      <section className="grid gap-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">Payment providers</h2>
            <p className="text-sm text-muted-foreground">
              Configure the payment rails this merchant can use.
            </p>
          </div>
          <ProviderDialog
            providerDefinitions={providerDefinitions}
            onDone={reload}
          />
        </div>
        {!data?.data?.length ? (
          <p className="py-2 text-sm text-muted-foreground">
            No payment providers configured.
          </p>
        ) : (
          <Table className="min-w-[44rem]">
            <TableHeader>
              <TableRow>
                <TableHead className="text-muted-foreground">
                  Provider
                </TableHead>
                <TableHead className="text-muted-foreground">
                  Environment
                </TableHead>
                <TableHead className="text-muted-foreground">
                  Credentials
                </TableHead>
                <TableHead className="text-muted-foreground">State</TableHead>
                <TableHead className="text-right text-muted-foreground">
                  Action
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.data.map((provider) => (
                <ProviderRow
                  key={provider.id}
                  provider={provider}
                  definition={providerDefinitions.find(
                    (definition) => definition.rail === provider.rail
                  )}
                  onDone={reload}
                />
              ))}
            </TableBody>
          </Table>
        )}
      </section>
    </div>
  )
}

function ProviderRow({
  provider,
  definition,
  onDone,
}: {
  provider: PaymentProviderConfig
  definition?: PaymentProviderDefinition
  onDone: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  return (
    <TableRow className={provider.archived ? "opacity-60" : undefined}>
      <TableCell className="py-3">
        <div className="grid min-w-0 leading-tight">
          <span className="font-medium">
            {definition?.display_name ?? provider.rail}
          </span>
          <span className="truncate text-xs text-muted-foreground">
            {provider.rail} · {provider.account_id}
          </span>
        </div>
      </TableCell>
      <TableCell className="py-3 capitalize">
        {provider.environment || "Default"}
      </TableCell>
      <TableCell className="py-3">
        {Object.keys(provider.credentials).length > 0 ? (
          <span className="flex flex-wrap gap-1">
            {Object.entries(provider.credentials).map(([name, credential]) => (
              <Badge
                key={name}
                variant="secondary"
                className={
                  credential.configured
                    ? ""
                    : "bg-amber-500/15 text-amber-600 dark:text-amber-400"
                }
                title={
                  credential.last_validated_at
                    ? `Validated ${formatDate(credential.last_validated_at)}`
                    : credential.configured
                      ? "Configured"
                      : "Not configured"
                }
              >
                {name}
              </Badge>
            ))}
          </span>
        ) : (
          <span className="text-sm text-muted-foreground">None required</span>
        )}
      </TableCell>
      <TableCell className="py-3">
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
      <TableCell className="py-3 text-right">
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
    customerID: string
    currency: string
    creditLimit: number
    trustLevel: string
  }>()
  const [newLimit, setNewLimit] = React.useState("")
  const [lookupBusy, setLookupBusy] = React.useState(false)
  const [saveBusy, setSaveBusy] = React.useState(false)
  const parsedNewLimit = microsFromInput(newLimit)
  const newLimitValid =
    newLimit !== "" && parsedNewLimit !== null && parsedNewLimit >= 0

  const lookup = async (event: React.FormEvent) => {
    event.preventDefault()
    const nextCustomerID = customerID.trim()
    const nextCurrency = currency.trim().toLowerCase()
    setLookupBusy(true)
    try {
      const [cl, tt] = await Promise.all([
        getCreditLimit(nextCustomerID, nextCurrency),
        getTrustLevel(nextCustomerID, nextCurrency),
      ])
      setResult({
        customerID: nextCustomerID,
        currency: cl.currency || nextCurrency,
        creditLimit: cl.credit_limit_amount,
        trustLevel: tt.trust_level,
      })
      setNewLimit("")
    } catch (err) {
      toastApiError(err, "Lookup customer controls")
      setResult(undefined)
    } finally {
      setLookupBusy(false)
    }
  }

  return (
    <div className="grid gap-10">
      <section className="grid gap-5">
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">Customer lookup</h2>
          <p className="text-sm text-muted-foreground">
            Find a customer by ID and currency.
          </p>
        </div>
        <form
          onSubmit={lookup}
          className="grid gap-4 md:grid-cols-[minmax(0,1fr)_10rem_auto] md:items-end"
        >
          <Field label="Customer ID" id="customer-controls-id">
            <Input
              id="customer-controls-id"
              placeholder="Customer UUID"
              value={customerID}
              onChange={(event) => setCustomerID(event.target.value)}
            />
          </Field>
          <Field label="Currency" id="customer-controls-currency">
            <Input
              id="customer-controls-currency"
              placeholder="USD"
              value={currency}
              onChange={(event) => setCurrency(event.target.value)}
            />
          </Field>
          <Button
            type="submit"
            variant="outline"
            disabled={
              lookupBusy || saveBusy || !customerID.trim() || !currency.trim()
            }
          >
            {lookupBusy ? "Looking up…" : "Look up"}
          </Button>
        </form>
      </section>

      {result && (
        <section className="grid gap-5">
          <div className="grid gap-1">
            <h2 className="text-base font-semibold">Credit and trust</h2>
            <p className="text-sm text-muted-foreground">
              <span className="font-mono text-xs break-all text-foreground">
                {result.customerID}
              </span>{" "}
              · {result.currency.toUpperCase()}
            </p>
          </div>
          <dl className="grid gap-4">
            <SettingDetail
              label="Trust level"
              value={result.trustLevel || "Default"}
            />
            <SettingDetail
              label="Credit limit"
              value={
                result.creditLimit
                  ? formatMicros(result.creditLimit, result.currency)
                  : "Off"
              }
            />
          </dl>
          <form
            onSubmit={async (event) => {
              event.preventDefault()
              if (
                newLimit === "" ||
                parsedNewLimit === null ||
                parsedNewLimit < 0
              )
                return
              setSaveBusy(true)
              try {
                await setCreditLimit(
                  result.customerID,
                  result.currency,
                  parsedNewLimit
                )
                setResult((current) =>
                  current
                    ? { ...current, creditLimit: parsedNewLimit }
                    : current
                )
                setNewLimit("")
                toast.success("Credit limit updated")
              } catch (err) {
                toastApiError(err, "Set credit limit")
              } finally {
                setSaveBusy(false)
              }
            }}
          >
            <SettingEditField
              label={`New limit (${result.currency.toUpperCase()})`}
              id="customer-controls-limit"
            >
              <div className="grid gap-1.5">
                <div className="flex gap-2">
                  <Input
                    id="customer-controls-limit"
                    placeholder="0.00"
                    type="number"
                    step="any"
                    min="0"
                    value={newLimit}
                    onChange={(event) => setNewLimit(event.target.value)}
                  />
                  <Button
                    type="submit"
                    disabled={saveBusy || lookupBusy || !newLimitValid}
                  >
                    {saveBusy ? "Updating…" : "Update"}
                  </Button>
                </div>
                <p className="text-xs text-muted-foreground">
                  Enter 0 to turn credit off.
                </p>
              </div>
            </SettingEditField>
          </form>
        </section>
      )}
    </div>
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
