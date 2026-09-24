// Card entry for the NMI rail. Live mode mounts Collect.js hosted iframes
// into these containers (card data never touches our code); preview mode
// (fixture tokenization keys) renders inert placeholders with the same
// geometry so every state is designable without a gateway.
import { Label } from "#orck/components/ui/label"
import type { CollectFieldErrors } from "#orck/lib/collect"
import { cn } from "cn"

function FieldError({ id, message }: { id: string; message?: string }) {
  return message ? (
    <p id={id} className="text-[12.5px] text-destructive" role="alert">
      {message}
    </p>
  ) : null
}

const FIELD =
  "orck-collect-field h-[38px] overflow-hidden rounded-[9px] border border-[color:var(--border)] bg-card px-[3px]"
const PREVIEW_FIELD =
  "flex h-[38px] items-center rounded-[9px] border border-[color:var(--border)] bg-card px-[11px] text-sm text-[color:var(--orck-faint)] tabular-nums"

export interface CardFieldIds {
  number: string
  expiry: string
  cvv: string
}

export function CardFields({
  ids,
  preview,
  error,
  fieldErrors = {},
  hidden,
}: {
  ids: CardFieldIds
  preview: boolean
  error?: string
  /** Inline per-field errors (gateway validation or a decline). */
  fieldErrors?: CollectFieldErrors
  // Kept mounted while hidden: the Collect.js iframes cannot be recreated
  // cheaply, so switching to a stored card must not unmount them.
  hidden?: boolean
}) {
  return (
    <div
      className={cn("grid gap-3", hidden && "hidden")}
      aria-hidden={hidden || undefined}
    >
      <div className="grid gap-1.5">
        <Label
          htmlFor={ids.number}
          className="text-[13px] font-medium text-foreground"
        >
          Card number
        </Label>
        {preview ? (
          <div id={ids.number} className={PREVIEW_FIELD}>
            1234 1234 1234 1234
          </div>
        ) : (
          <div
            id={ids.number}
            className={cn(FIELD, fieldErrors.number && "border-destructive")}
          />
        )}
        <FieldError id={`${ids.number}-error`} message={fieldErrors.number} />
      </div>
      <div className="grid grid-cols-2 gap-3">
        <div className="grid gap-1.5">
          <Label
            htmlFor={ids.expiry}
            className="text-[13px] font-medium text-foreground"
          >
            Expiry
          </Label>
          {preview ? (
            <div id={ids.expiry} className={PREVIEW_FIELD}>
              MM / YY
            </div>
          ) : (
            <div
              id={ids.expiry}
              className={cn(FIELD, fieldErrors.expiry && "border-destructive")}
            />
          )}
          <FieldError id={`${ids.expiry}-error`} message={fieldErrors.expiry} />
        </div>
        <div className="grid gap-1.5">
          <Label
            htmlFor={ids.cvv}
            className="text-[13px] font-medium text-foreground"
          >
            CVC
          </Label>
          {preview ? (
            <div id={ids.cvv} className={PREVIEW_FIELD}>
              CVC
            </div>
          ) : (
            <div
              id={ids.cvv}
              className={cn(FIELD, fieldErrors.cvv && "border-destructive")}
            />
          )}
          <FieldError id={`${ids.cvv}-error`} message={fieldErrors.cvv} />
        </div>
      </div>
      {error ? (
        <p className={cn("text-[13px] text-destructive")} role="alert">
          {error}
        </p>
      ) : null}
    </div>
  )
}
