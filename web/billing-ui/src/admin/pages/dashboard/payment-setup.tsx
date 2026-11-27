import { AdminMessage } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { useQuery } from "@tanstack/react-query"

import { useNavigate } from "#orck/admin/router"
import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardAction,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import { useMerchantConfig } from "#orck/admin/lib/capabilities"

// PaymentSetup is the merchant's setup step while none of its PSPs is armed:
// until one is, no customer can pay. Where Vault holds the configuration it
// leads to the PSPs page; read from a file, it says what to declare there.
export function PaymentSetup() {
  const { adminQueries } = useAdminServices()
  const readable = useMerchantConfig()
  const navigate = useNavigate()
  const psps = useQuery({ ...adminQueries.psps(), enabled: readable })
  const config = useQuery(adminQueries.config())
  if (!readable || !psps.data || !config.data) return null
  if (psps.data.data.some((psp) => psp.armed)) return null
  const edits = !!config.data.capabilities.features?.merchant_config_edits
  return (
    <Card data-setup="psp">
      <CardHeader>
        <CardTitle className="text-base font-semibold">
          <AdminMessage id={"Connect a payment provider to take payments"} />
        </CardTitle>
        <CardDescription>
          <AdminMessage id={"No PSP is armed, so no customer can pay yet. "} />
          {edits ? (
            <AdminMessage
              id={
                "Add one with its credentials: OpenRails checks them with the provider before it arms it."
              }
            />
          ) : (
            <>
              <AdminMessage
                id={
                  "This merchant's configuration is read from a file: declare a PSP in it under "
                }
              />
              <code className="whitespace-nowrap">psps.&lt;key&gt;</code>
              <AdminMessage
                id={
                  " (its rail, environment, account and secrets), then restart the server."
                }
              />
            </>
          )}
        </CardDescription>
        {edits && (
          <CardAction>
            <Button size="sm" onClick={() => navigate("/settings?tab=psps")}>
              <AdminMessage id={"Connect a payment provider"} />
            </Button>
          </CardAction>
        )}
      </CardHeader>
    </Card>
  )
}
