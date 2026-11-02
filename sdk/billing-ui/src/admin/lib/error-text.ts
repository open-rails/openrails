import { ApiError } from "#orck/admin/lib/api/client"
import { en } from "../../locales/en"

// Staff wording for refusals the shared customer strings do not cover.
const staffErrors: Record<string, string> = {
  destructive_actions_held:
    "Refunds, revokes and cancels are paused for you after several in a short time. Someone else can clear the hold on the Ops page.",
  authorization_unavailable:
    "Billing couldn't check your permissions just now. Try again in a moment.",
  permission_required: "Your role doesn't allow this.",
}

const sentence = (text: string) => {
  const trimmed = text.trim()
  if (!trimmed) return trimmed
  const first = trimmed.charAt(0).toUpperCase() + trimmed.slice(1)
  return /[.!?]$/.test(first) ? first : `${first}.`
}

// errorText is a failure as a plain sentence for staff, with the API's code
// kept apart for a secondary detail line, never the main text.
export function errorText(error: unknown): { text: string; code?: string } {
  if (!(error instanceof ApiError))
    return {
      text: sentence(
        error instanceof Error && error.message
          ? error.message
          : en.errors.generic
      ),
    }
  const code = error.code
  const known =
    (code &&
      (staffErrors[code] ?? (en.errors as Record<string, string>)[code])) ||
    (error.status === 404 ? en.errors.resource_not_found : undefined) ||
    (error.status === 403 ? staffErrors.permission_required : undefined)
  return {
    text: known ?? sentence(error.message || en.errors.generic),
    code,
  }
}
