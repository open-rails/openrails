// A dialog never outgrows a short phone screen: it scrolls inside itself, so
// its buttons stay reachable.
export const DIALOG_FIT = "max-h-[calc(100dvh-2rem)] overflow-y-auto"

// One width scale for every dialog. Always prefix with `sm:`: a bare `max-w-*`
// overrides the base `max-w-[calc(100%-2rem)]` cap and lets the dialog reach
// the screen edge on a phone.

/** Destructive confirmations: a sentence, sometimes a typed slug. */
export const DIALOG_CONFIRM = `sm:max-w-md ${DIALOG_FIT}`

/** The default. Anything a merchant fills in: money, dates, references. */
export const DIALOG_FORM = `sm:max-w-xl ${DIALOG_FIT}`

/** Forms carrying a live preview or a document alongside the fields. */
export const DIALOG_WIDE = `sm:max-w-2xl ${DIALOG_FIT}`
