import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon } from "@hugeicons/core-free-icons"
import { useNavigate } from "react-router-dom"

import { LogoLockup } from "@/components/logo"
import { PendingInvites } from "@/components/pending-invites"
import { Button } from "@/components/ui/button"
import { newMerchantPath, useExtensions } from "@/extensions/registry"
import { useAuth } from "@/lib/auth"
import { cn } from "@/lib/utils"

// NoMerchants is the console for a user who belongs to no merchant. Standalone
// has no self-service creation, so it points at an operator; a host extension
// that creates merchants (newMerchantPath) offers its page. Inside the shell
// (a host with its own user pages) it is the merchant pages' content.
export function NoMerchants({ inShell = false }: { inShell?: boolean }) {
  const { me, logout, federated } = useAuth()
  const navigate = useNavigate()
  const newMerchant = newMerchantPath(useExtensions().extensions)

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
          You don&apos;t have access to any merchants yet
        </h1>
        <p className="mt-2 text-sm text-pretty text-muted-foreground">
          {newMerchant
            ? "Create a merchant to start accepting payments, or ask a teammate to add you to theirs."
            : "Ask an operator of this OpenRails deployment to add you to a merchant."}
        </p>
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
        {federated && <PendingInvites />}
        {!inShell && me?.email && (
          <p className="mt-6 text-xs text-muted-foreground">
            Signed in as {me.email}
          </p>
        )}
      </div>
    </div>
  )
}
