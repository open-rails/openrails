// The one bold element on the surface: the ink button carrying the amount.
// Redirect rails relabel it; Solana has no button at all.
import { Button } from "#orck/components/ui/button"
import { cn } from "cn"

export function PayButton({
  label,
  processing,
  disabled,
}: {
  label: string
  processing: boolean
  disabled?: boolean
}) {
  return (
    <Button
      type="submit"
      className={cn(
        "h-[42px] w-full rounded-[10px] text-sm font-semibold tabular-nums",
        processing && "opacity-75"
      )}
      disabled={disabled || processing}
    >
      {processing ? (
        <>
          <span
            aria-hidden
            className="size-3.5 animate-spin rounded-full border-2 border-[color:var(--primary-foreground)]/35 border-t-[color:var(--primary-foreground)] motion-reduce:animate-none"
          />
          Processing…
        </>
      ) : (
        label
      )}
    </Button>
  )
}

export function TrustLine() {
  return (
    <div className="-mt-1 flex items-center justify-center gap-1.5 text-xs text-[color:var(--orck-faint)]">
      <svg
        viewBox="0 0 16 16"
        fill="currentColor"
        aria-hidden
        className="size-[11px]"
      >
        <path d="M8 1a3.5 3.5 0 0 0-3.5 3.5V6H4a1.5 1.5 0 0 0-1.5 1.5v5A1.5 1.5 0 0 0 4 14h8a1.5 1.5 0 0 0 1.5-1.5v-5A1.5 1.5 0 0 0 12 6h-.5V4.5A3.5 3.5 0 0 0 8 1Zm2 5H6V4.5a2 2 0 1 1 4 0V6Z" />
      </svg>
      Secured by OpenRails
    </div>
  )
}
