import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useAdminServices } from "#orck/admin/context"
import { useMerchantConfigEdits } from "#orck/admin/lib/capabilities"
import { Button } from "#orck/admin/components/ui/button"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import type { Custodian } from "#orck/client/generated/wire"

// Custody and charging remain independent choices. Selecting a new-card vault
// never rewrites the saved instrument or mandate on an existing subscription.
export function CustodySettings() {
  const services = useAdminServices()
  const client = useQueryClient()
  const edits = useMerchantConfigEdits()
  const configuration = useQuery(services.adminQueries.merchantConfiguration())
  const psps = useQuery(services.adminQueries.psps())
  const key = [...services.queryKeys.settings(), "custodians"]
  const custodians = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => services.listCustodians(signal),
  })
  const [editing, setEditing] = React.useState<Custodian | "new" | null>(null)
  const [merchant] = React.useState(() => services.selectedMerchant())
  const refresh = () =>
    client.invalidateQueries({ queryKey: services.queryKeys.settings() })
  const update = useMutation({
    mutationFn: async ({ psp, custody }: { psp: string; custody: string }) => {
      if (merchant !== services.selectedMerchant())
        throw new Error("Merchant changed; reopen settings")
      const current = psps.data?.data.find((p) => p.id === psp)
      if (!current) throw new Error("PSP is no longer available")
      return services.updatePSP(psp, {
        expected_revision: current.revision,
        custodian: custody,
      })
    },
    onSuccess: refresh,
    onError: (error) => services.toastApiError(error, "Change card custody"),
  })
  const routing = useMutation({
    mutationFn: async (form: FormData) => {
      if (!configuration.data || merchant !== services.selectedMerchant())
        throw new Error("Merchant changed; reopen settings")
      return services.updateMerchantConfiguration({
        expected_revision: configuration.data.revision,
        card_vault: String(form.get("card_vault") ?? ""),
        charge_psps: form.getAll("charge_psps").map(String),
      })
    },
    onSuccess: refresh,
    onError: (error) => services.toastApiError(error, "Save card routing"),
  })
  if (!configuration.data || !psps.data || !custodians.data) return null
  const active = psps.data.data.filter(
    (p) => !p.archived && (p.rail === "nmi" || p.rail === "stripe")
  )
  return (
    <section className="grid gap-5" aria-label="Card vaults">
      <div>
        <h2 className="text-base font-semibold">Card vaults</h2>
        <p className="text-sm text-muted-foreground">
          Choose where new cards are stored and which accounts charge them.
          Existing cards and subscriptions keep their accepted custody.
        </p>
      </div>
      <ul className="grid gap-2">
        {custodians.data.data.map((vault) => (
          <li
            key={vault.key}
            className="flex flex-wrap items-center justify-between gap-3 rounded border p-3"
          >
            <div>
              <strong>{vault.key}</strong> · Basis Theory · {vault.environment}
              <br />
              <span className="text-sm text-muted-foreground">
                {vault.account_id} ·{" "}
                {vault.credentials?.api_key?.configured
                  ? "Credential configured"
                  : "Credential missing"}
              </span>
            </div>
            {edits && !vault.archived && (
              <Button
                size="sm"
                variant="outline"
                onClick={() => setEditing(vault)}
              >
                Edit vault
              </Button>
            )}
          </li>
        ))}
      </ul>
      {edits && (
        <Button
          className="justify-self-start"
          variant="outline"
          onClick={() => setEditing("new")}
        >
          Add Basis Theory vault
        </Button>
      )}
      {editing && (
        <CustodianForm
          key={editing === "new" ? "new" : `${editing.key}:${editing.revision}`}
          current={editing === "new" ? undefined : editing}
          close={() => setEditing(null)}
          saved={refresh}
        />
      )}
      {active
        .filter((p) => p.rail === "nmi")
        .map((psp) => (
          <Label key={psp.id} className="grid gap-2">
            {psp.key}: card storage
            <select
              className="rounded border bg-background p-2"
              aria-label={`${psp.key} card storage`}
              disabled={!edits || update.isPending}
              value={psp.custodian ?? ""}
              onChange={(e) =>
                update.mutate({ psp: psp.id, custody: e.target.value })
              }
            >
              <option value="">NMI customer vault</option>
              {custodians.data.data
                .filter((v) => !v.archived && v.environment === psp.environment)
                .map((v) => (
                  <option key={v.key} value={v.key}>
                    {v.key} · Basis Theory
                  </option>
                ))}
            </select>
          </Label>
        ))}
      <form
        key={`routing:${configuration.data.revision}`}
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          routing.mutate(new FormData(e.currentTarget))
        }}
      >
        <Label className="grid gap-2">
          Vault for new cards
          <select
            name="card_vault"
            className="rounded border bg-background p-2"
            defaultValue={configuration.data.card_vault ?? ""}
            disabled={!edits}
          >
            <option value="">No card vault selected</option>
            {configuration.data.platform_vault.available && (
              <option value="platform">
                Hosted vault · isolated to this merchant
              </option>
            )}
            {active.map((p) => (
              <option key={p.id} value={p.key}>
                {p.key} ·{" "}
                {p.custodian
                  ? "Basis Theory"
                  : p.rail === "stripe"
                    ? "Stripe"
                    : "NMI customer vault"}
              </option>
            ))}
          </select>
        </Label>
        <fieldset className="grid gap-2" disabled={!edits}>
          <legend className="mb-2 font-medium">Charge PSPs</legend>
          <p className="text-sm text-muted-foreground">
            Select the accounts allowed for new charges. With none selected,
            routing considers all eligible accounts.
          </p>
          {active.map((p) => (
            <Label key={p.id} className="flex gap-2">
              <input
                type="checkbox"
                name="charge_psps"
                value={p.key}
                defaultChecked={
                  configuration.data?.charge_psps?.includes(p.key) ?? false
                }
              />
              {p.key}
            </Label>
          ))}
        </fieldset>
        {edits && (
          <Button
            className="justify-self-start"
            disabled={routing.isPending}
            type="submit"
          >
            Save card routing
          </Button>
        )}
      </form>
    </section>
  )
}

function CustodianForm({
  current,
  close,
  saved,
}: {
  current?: Custodian
  close: () => void
  saved: () => Promise<unknown>
}) {
  const services = useAdminServices()
  const [merchant] = React.useState(() => services.selectedMerchant())
  const mutation = useMutation({
    mutationFn: async (form: FormData) => {
      if (merchant !== services.selectedMerchant())
        throw new Error("Merchant changed; reopen this form")
      const secret = String(form.get("private_key") ?? "").trim()
      return services.putCustodian(
        current?.key ?? String(form.get("key") ?? "").trim(),
        {
          expected_revision: current?.revision ?? 0,
          kind: "basis_theory",
          environment:
            current?.environment ?? String(form.get("environment") ?? "test"),
          account_id:
            current?.account_id ?? String(form.get("account_id") ?? "").trim(),
          settings: {
            ...(current?.settings ?? {}),
            public_api_key: String(form.get("public_key") ?? "").trim(),
          },
          ...(secret ? { credentials: { api_key: secret } } : {}),
        }
      )
    },
    onSuccess: async () => {
      await saved()
      close()
    },
    onError: (error) => services.toastApiError(error, "Configure vault"),
  })
  return (
    <form
      className="grid gap-3 rounded border p-4"
      onSubmit={(e) => {
        e.preventDefault()
        mutation.mutate(new FormData(e.currentTarget))
      }}
    >
      <h3 className="font-medium">
        {current ? `Edit ${current.key}` : "Add Basis Theory vault"}
      </h3>
      {!current && (
        <>
          <Label className="grid gap-1">
            Vault key
            <Input name="key" required pattern="[a-z0-9_-]{1,63}" />
          </Label>
          <Label className="grid gap-1">
            Environment
            <select
              name="environment"
              className="rounded border bg-background p-2"
            >
              <option value="test">Test</option>
              <option value="live">Live</option>
            </select>
          </Label>
          <Label className="grid gap-1">
            Basis Theory tenant ID
            <Input name="account_id" required />
          </Label>
        </>
      )}
      <Label className="grid gap-1">
        Public collection key
        <Input
          name="public_key"
          required
          defaultValue={String(current?.settings?.public_api_key ?? "")}
        />
      </Label>
      <Label className="grid gap-1">
        Private API key
        <Input
          type="password"
          name="private_key"
          autoComplete="new-password"
          required={!current}
        />
      </Label>
      <p className="text-sm text-muted-foreground">
        Use a key that can read its application, manage the merchant’s card
        tokens and invoke the proxy. Existing credentials stay unchanged when
        left blank.
      </p>
      <div className="flex gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          Validate and save vault
        </Button>
        <Button type="button" variant="outline" onClick={close}>
          Cancel
        </Button>
      </div>
    </form>
  )
}
