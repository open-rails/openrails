// How long a product-access grant lasts: a preset number of days (added after
// any access the customer already has), until a date, or forever.

// MAX_GRANT_HOURS is the longest grant the API takes: the most hours a Go
// time.Duration holds.
export const MAX_GRANT_HOURS = 2_562_047

export const GRANT_DURATIONS = [
  { value: "1", label: "1 day", days: 1 },
  { value: "7", label: "7 days", days: 7 },
  { value: "30", label: "30 days", days: 30 },
  { value: "365", label: "365 days", days: 365 },
  { value: "until", label: "Until a date" },
  { value: "never", label: "Never expires" },
] as const

export type GrantDuration = (typeof GRANT_DURATIONS)[number]["value"]

export const DEFAULT_GRANT_DURATION: GrantDuration = "30"

export type GrantTerm = { hours: number } | { endsAt: string } | object

const today = (now: Date) => now.toISOString().slice(0, 10)

// grantTerm is what the grant request carries for a choice. A date ends the
// grant at the close of that UTC day, sent as an instant.
export function grantTerm(
  choice: GrantDuration,
  until: string,
  now = new Date()
): GrantTerm | null {
  if (choice === "never") return {}
  if (choice === "until") {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(until) || until < today(now)) return null
    const end = new Date(`${until}T00:00:00Z`)
    end.setUTCDate(end.getUTCDate() + 1)
    return { endsAt: end.toISOString() }
  }
  const preset = GRANT_DURATIONS.find((d) => d.value === choice)
  return preset && "days" in preset ? { hours: preset.days * 24 } : null
}
