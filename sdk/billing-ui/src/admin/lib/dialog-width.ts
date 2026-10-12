// One width scale for every dialog. Always prefix with `sm:`: a bare `max-w-*`
// overrides the base `max-w-[calc(100%-2rem)]` cap and lets the dialog reach
// the screen edge on a phone.

/** Destructive confirmations: a sentence, sometimes a typed slug. */
export const DIALOG_CONFIRM = "sm:max-w-md"

/** The default. Anything a merchant fills in: money, dates, references. */
export const DIALOG_FORM = "sm:max-w-xl"

/** Forms carrying a live preview or a document alongside the fields. */
export const DIALOG_WIDE = "sm:max-w-2xl"
