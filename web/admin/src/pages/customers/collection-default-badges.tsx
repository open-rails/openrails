import { Badge } from "@/components/ui/badge"

// Currency names come from the collection policy on the server. Subscription
// defaults and the order of saved methods never imply a collection default.
export function CollectionDefaultBadges({
  currencies = [],
}: {
  currencies?: string[]
}) {
  return (
    <>
      {currencies.map((currency) => (
        <Badge key={currency} variant="secondary" className="mt-1 mr-1">
          Collection default · {currency}
        </Badge>
      ))}
    </>
  )
}
