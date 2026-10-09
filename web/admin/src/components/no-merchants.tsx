import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon } from "@hugeicons/core-free-icons"

import { LogoLockup } from "@/components/logo"
import { PendingInvites } from "@/components/pending-invites"
import { Button } from "@/components/ui/button"
import { getBootstrap } from "@/lib/api/client"
import { useAuth } from "@/lib/auth"

// NoMerchants is the console for a user who belongs to no merchant. Standalone
// has no self-service creation, so it points at an operator; a host that
// creates merchants (AdminConsoleConfig.NewMerchantURL) offers its own page.
export function NoMerchants() {
  const { me, logout, federated } = useAuth()
  const newMerchantURL = getBootstrap().new_merchant_url

  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-6 py-12">
      <div className="w-full max-w-sm">
        <LogoLockup className="h-4" />
        <h1 className="mt-8 text-2xl font-semibold tracking-tight text-balance">
          You don&apos;t have access to any merchants yet
        </h1>
        <p className="mt-2 text-sm text-pretty text-muted-foreground">
          {newMerchantURL
            ? "Create a merchant to start accepting payments, or ask a teammate to add you to theirs."
            : "Ask an operator of this OpenRails deployment to add you to a merchant."}
        </p>
        <div className="mt-8 flex flex-wrap items-center gap-2">
          {newMerchantURL && (
            <Button onClick={() => window.location.assign(newMerchantURL)}>
              <HugeiconsIcon icon={Add01Icon} />
              New merchant
            </Button>
          )}
          <Button variant="ghost" onClick={() => void logout()}>
            Sign out
          </Button>
        </div>
        {federated && <PendingInvites />}
        {me?.email && (
          <p className="mt-6 text-xs text-muted-foreground">
            Signed in as {me.email}
          </p>
        )}
      </div>
    </div>
  )
}
