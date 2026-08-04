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

export function SettingsPage() {
  return (
    <Tabs defaultValue="merchant" className="flex flex-col gap-4">
      <TabsList>
        <TabsTrigger value="merchant">Merchant</TabsTrigger>
        <TabsTrigger value="team">Team</TabsTrigger>
        <TabsTrigger value="alerts">Alerts</TabsTrigger>
        <TabsTrigger value="providers">Payment providers</TabsTrigger>
        <TabsTrigger value="api-keys">API keys</TabsTrigger>
        <TabsTrigger value="customer-controls">Customer controls</TabsTrigger>
      </TabsList>
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
    <div className="grid gap-4">
      <MerchantProfileForm initial={data?.profile} />
      <RepriceNoticeWindowForm initial={data?.reprice_notice_window_days} />
    </div>
  )
}

function MerchantProfileForm({
  initial,
}: {
  initial?: { display_name?: string; from_email?: string; support_url?: string; logo_url?: string }
}) {
  const [displayName, setDisplayName] = React.useState(initial?.display_name ?? "")
  const [fromEmail, setFromEmail] = React.useState(initial?.from_email ?? "")
  const [supportURL, setSupportURL] = React.useState(initial?.support_url ?? "")
  const [logoURL, setLogoURL] = React.useState(initial?.logo_url ?? "")
  const [busy, setBusy] = React.useState(false)

  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle className="text-sm">Merchant profile</CardTitle>
        <CardDescription>Shown on invoices and customer-facing emails.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <Field label="Display name" id="s-name">
          <Input id="s-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
        </Field>
        <Field label="From email" id="s-email">
          <Input id="s-email" type="email" value={fromEmail} onChange={(e) => setFromEmail(e.target.value)} />
        </Field>
        <Field label="Support URL" id="s-support">
          <Input id="s-support" value={supportURL} onChange={(e) => setSupportURL(e.target.value)} />
        </Field>
        <Field label="Logo URL" id="s-logo">
          <Input id="s-logo" value={logoURL} onChange={(e) => setLogoURL(e.target.value)} />
        </Field>
        <div>
          <Button
            disabled={busy}
            onClick={async () => {
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
                toast.success("Settings saved")
              } catch (err) {
                toastApiError(err, "Save settings")
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

// RepriceNoticeWindowForm (#781): the merchant-configurable minimum advance
// notice (days) a subscription price INCREASE must give existing
// subscribers. The catalog price-change wizard reads this same value
// (GET /v1/merchant/settings) for its own date-picker gate; the API enforces
// it regardless of what the console shows.
function RepriceNoticeWindowForm({ initial }: { initial?: number }) {
  const [days, setDays] = React.useState(String(initial ?? 30))
  const [busy, setBusy] = React.useState(false)
  const parsed = Number(days)
  const valid = days.trim() !== "" && Number.isInteger(parsed) && parsed >= 0

  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle className="text-sm">Price-increase notice window</CardTitle>
        <CardDescription>
          Minimum days' advance notice a scheduled subscription price INCREASE must give existing
          subscribers before it takes effect. Decreases are never gated. Default 30 days.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <Field label="Notice window (days)" id="s-notice-window">
          <Input
            id="s-notice-window"
            type="number"
            step="1"
            min="0"
            value={days}
            onChange={(e) => setDays(e.target.value)}
          />
        </Field>
        <div>
          <Button
            disabled={busy || !valid}
            onClick={async () => {
              setBusy(true)
              try {
                await putMerchantSettings({ reprice_notice_window_days: parsed })
                toast.success("Settings saved")
              } catch (err) {
                toastApiError(err, "Save settings")
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

function ProvidersTab() {
  const { data, loading, error, reload } = useApiData(() => listPaymentProviders(), [])
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
        <p className="text-sm text-muted-foreground">No payment providers configured.</p>
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

function ProviderRow({ provider, onDone }: { provider: PaymentProviderConfig; onDone: () => void }) {
  const [busy, setBusy] = React.useState(false)
  return (
    <TableRow className={provider.archived ? "opacity-60" : undefined}>
      <TableCell className="font-medium">{provider.rail}</TableCell>
      <TableCell>{provider.environment}</TableCell>
      <TableCell className="font-mono text-xs">{provider.account_id}</TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {Object.entries(provider.credentials).map(([name, c]) => (
            <Badge
              key={name}
              variant="secondary"
              className={c.configured ? "" : "bg-amber-500/15 text-amber-600 dark:text-amber-400"}
              title={c.last_validated_at ? `validated ${formatDate(c.last_validated_at)}` : undefined}
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
          <Badge variant="secondary" className="bg-amber-500/15 text-amber-600 dark:text-amber-400">
            draining ({provider.open_obligations})
          </Badge>
        ) : (
          <Badge variant="secondary" className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400">
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
  const [credentials, setCredentials] = React.useState<Record<string, string>>(
    {}
  )
  const [busy, setBusy] = React.useState(false)
  const selectedProvider = providerDefinitions.find(
    (provider) => provider.rail === rail
  )
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm">Configure provider</Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Configure payment provider</DialogTitle>
          <DialogDescription>
            account_id is operator-declared per rail (NMI gateway id, Stripe
            acct_…, CCBill clientAccnum-clientSubacc, Solana wallet). The
            environment follows the deployment's test_mode. Credentials are
            stored in the secret backend.
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
            <Input id="pv-acct" value={accountID} onChange={(e) => setAccountID(e.target.value)} />
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
  const [result, setResult] = React.useState<{ creditLimit: number; trustLevel: string }>()
  const [newLimit, setNewLimit] = React.useState("")
  const [busy, setBusy] = React.useState(false)

  const lookup = async () => {
    setBusy(true)
    try {
      const [cl, tt] = await Promise.all([
        getCreditLimit(customerID.trim(), currency.trim()),
        getTrustLevel(customerID.trim(), currency.trim()),
      ])
      setResult({ creditLimit: cl.credit_limit_amount, trustLevel: tt.trust_level })
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
          Per-customer, per-currency: the arrears credit limit is writable; the trust level
          is graduated by spend history (read-only here).
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="grid grid-cols-[1fr_8rem_auto] gap-2">
          <Input placeholder="customer id (UUID)" value={customerID} onChange={(e) => setCustomerID(e.target.value)} />
          <Input placeholder="currency" value={currency} onChange={(e) => setCurrency(e.target.value)} />
          <Button variant="outline" disabled={busy || !customerID.trim() || !currency.trim()} onClick={lookup}>
            Look up
          </Button>
        </div>
        {result && (
          <div className="grid gap-3 rounded-md border p-3 text-sm">
            <p>
              Trust level: <Badge variant="secondary">{result.trustLevel || "default"}</Badge>
            </p>
            <p>
              Credit limit:{" "}
              {result.creditLimit ? formatMicros(result.creditLimit, currency) : "off (0)"}
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
                disabled={busy || newLimit === "" || microsFromInput(newLimit) === null}
                onClick={async () => {
                  setBusy(true)
                  try {
                    await setCreditLimit(customerID.trim(), currency.trim(), microsFromInput(newLimit) ?? 0)
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

function Field({ label, id, children }: { label: string; id: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}
