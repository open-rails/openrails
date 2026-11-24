// A card the customer has already stored with this merchant, as the card
// pickers show it. Display data only: paying with one sends its id.
export interface SavedPaymentMethod {
  id: string
  /** The PSP that holds the card. */
  psp_id: string
  rail: string
  card?: {
    brand?: string | null
    last4?: string | null
    exp_month?: number | null
    exp_year?: number | null
  } | null
}
