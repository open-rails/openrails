import { AdminMessage } from "#orck/admin/ui-context"
import { Badge } from "#orck/admin/components/ui/badge"

// The currencies the card is the customer's default for: it pays their
// usage orders there and every subscription without a card of its own.
export function DefaultCardBadges({
  currencies = [],
}: {
  currencies?: string[]
}) {
  return (
    <>
      {currencies.map((currency) => (
        <Badge key={currency} variant="secondary" className="mt-1 mr-1">
          <AdminMessage id={"Default · "} />
          {currency}
        </Badge>
      ))}
    </>
  )
}
