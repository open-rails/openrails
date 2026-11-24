import { cn } from "cn"

/** A small uppercase label above a section. */
export function Eyebrow({
  children,
  className,
}: {
  children: React.ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        "text-[11px] font-semibold tracking-[0.09em] text-muted-foreground uppercase",
        className
      )}
    >
      {children}
    </div>
  )
}
