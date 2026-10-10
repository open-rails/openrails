import * as React from "react"
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon } from "@hugeicons/core-free-icons"
import { useNavigate } from "react-router-dom"

import { LogoLockup } from "@/components/logo"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { newMerchantPath, useExtensions } from "@/extensions/registry"
import { useAuth } from "@/lib/auth"
import { cn } from "@/lib/utils"

// NoMerchants is the console with no merchant selected: staff open one by name
// (no host directory or mount merchant; the admin API decides access), else a
// host's newMerchantPath is offered. inShell renders it inside a host shell.
export function NoMerchants({ inShell = false }: { inShell?: boolean }) {
  const { me, logout, opensByName, selectMerchant } = useAuth()
  const navigate = useNavigate()
  const { extensions } = useExtensions()
  const newMerchant = newMerchantPath(extensions)
  const [name, setName] = React.useState("")

  return (
    <div
      className={cn(
        "flex items-center justify-center",
        inShell ? "py-16" : "min-h-svh bg-background px-6 py-12"
      )}
    >
      <div className="w-full max-w-sm">
        {!inShell && <LogoLockup className="h-4" />}
        <h1
          className={cn(
            "text-2xl font-semibold tracking-tight text-balance",
            !inShell && "mt-8"
          )}
        >
          {opensByName
            ? "Open a merchant"
            : "You don't have access to any merchants yet"}
        </h1>
        <p className="mt-2 text-sm text-pretty text-muted-foreground">
          {opensByName
            ? "Enter the name of the merchant you work on. An operator of this OpenRails deployment grants the access."
            : newMerchant
              ? "Create a merchant to start accepting payments, or ask a teammate to add you to theirs."
              : "Ask an operator of this OpenRails deployment to add you to a merchant."}
        </p>
        {opensByName && (
          <form
            className="mt-6 grid gap-2"
            onSubmit={(event) => {
              event.preventDefault()
              const slug = name.trim().toLowerCase()
              if (slug) selectMerchant(slug)
            }}
          >
            <Label htmlFor="merchant-name">Merchant name</Label>
            <div className="flex gap-2">
              <Input
                id="merchant-name"
                value={name}
                autoComplete="off"
                onChange={(event) => setName(event.target.value)}
              />
              <Button type="submit" disabled={!name.trim()}>
                Open
              </Button>
            </div>
          </form>
        )}
        <div className="mt-8 flex flex-wrap items-center gap-2">
          {newMerchant && (
            <Button onClick={() => void navigate(newMerchant)}>
              <HugeiconsIcon icon={Add01Icon} />
              New merchant
            </Button>
          )}
          {!inShell && (
            <Button variant="ghost" onClick={() => void logout()}>
              Sign out
            </Button>
          )}
        </div>
        {extensions.map(
          (extension) =>
            extension.EmptyState && <extension.EmptyState key={extension.id} />
        )}
        {!inShell && me?.email && (
          <p className="mt-6 text-xs text-muted-foreground">
            Signed in as {me.email}
          </p>
        )}
      </div>
    </div>
  )
}
