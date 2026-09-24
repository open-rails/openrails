// Terminal and transitional views. Each is quiet: one fact, one instruction,
// no dead controls.
import { Skeleton } from "#orck/components/ui/skeleton"
import { Eyebrow } from "#orck/components/summary"

export function LoadingSkeleton() {
  return (
    <div className="grid gap-4" aria-hidden>
      <Skeleton className="h-3 w-24" />
      <Skeleton className="h-6 w-52" />
      <Skeleton className="mt-1.5 h-11 w-full" />
      <Skeleton className="h-28 w-full" />
      <Skeleton className="h-11 w-full" />
      <Skeleton className="mt-1 h-[42px] w-full" />
    </div>
  )
}

export function SucceededView({
  merchantName,
  embedded,
}: {
  merchantName: string
  embedded: boolean
}) {
  return (
    <div className="grid gap-4">
      <Eyebrow>{merchantName}</Eyebrow>
      <div className="grid justify-items-center gap-2.5 px-2 py-7 text-center">
        <div className="grid size-11 place-items-center rounded-full bg-[color:var(--orck-success)]/12 text-[color:var(--orck-success)]">
          <svg
            width="20"
            height="20"
            viewBox="0 0 20 20"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.2"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden
          >
            <path d="M4 10.5l4 4 8-9" />
          </svg>
        </div>
        <div className="text-[15px] font-semibold">Payment complete</div>
        <div className="text-[13px] text-muted-foreground">
          {embedded ? "You're all set." : `Returning to ${merchantName}…`}
        </div>
      </div>
    </div>
  )
}

export function TerminalView({
  merchantName,
  headline,
  sub,
}: {
  merchantName: string
  headline: string
  sub?: string
}) {
  return (
    <div className="grid gap-4">
      <Eyebrow>{merchantName}</Eyebrow>
      <div className="grid justify-items-center gap-2 px-2 py-7 text-center">
        <div className="text-[15px] font-semibold">{headline}</div>
        {sub ? (
          <div className="text-[13px] text-muted-foreground">{sub}</div>
        ) : null}
      </div>
    </div>
  )
}
