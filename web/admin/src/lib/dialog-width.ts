// One width scale for every dialog in the console.
//
// The shared DialogContent defaults to `sm:max-w-sm` (384px), which is a
// confirmation width being used for data entry: amount, currency, reference and
// note fields all had to stack in a column narrower than a phone. These three
// tokens replace that per-dialog guesswork.
//
// Always prefix with `sm:`. The base component caps width at
// `max-w-[calc(100%-2rem)]` so a dialog keeps a gutter on small screens; a bare
// `max-w-lg` overrides that cap at every size and lets the dialog reach the
// screen edge on a phone.

/** Destructive confirmations: a sentence, sometimes a typed slug. */
export const DIALOG_CONFIRM = "sm:max-w-md"

/** The default. Anything a merchant fills in: money, dates, references. */
export const DIALOG_FORM = "sm:max-w-xl"

/** Forms carrying a live preview or a document alongside the fields. */
export const DIALOG_WIDE = "sm:max-w-2xl"
