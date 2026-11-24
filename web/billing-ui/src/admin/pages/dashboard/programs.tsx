import { useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// The dispute program standings (GET /v1/metrics/dispute-programs): each card
// PSP's ratio in each monitoring program for the month holding at, projected
// over the month, against the program's levels.
import { useQuery } from "@tanstack/react-query"
import { Badge } from "#orck/admin/components/ui/badge"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import type { DisputeStanding } from "#orck/admin/lib/api/metrics"
import { errorText } from "#orck/admin/lib/error-text"
import { cn } from "#orck/admin/lib/utils"

import { usePSPKeys } from "./widget-viz"

const PROGRAMS: Record<DisputeStanding["program"], string> = {
  visa_vamp: "Visa VAMP",
  mastercard_ecp: "Mastercard ECM",
  mastercard_efm: "Mastercard EFM",
  stripe: "Stripe",
  acquirer: "Acquirer limit",
}

const LEVELS: Record<DisputeStanding["level"], string> = {
  none: "Below",
  warning: "Warning",
  excessive: "Excessive",
  high_excessive: "High excessive",
}

const percent = (ratio: number) => `${(ratio * 100).toFixed(2)}%`

// The program's levels and their ratios.
function levels(standing: DisputeStanding): string {
  return standing.thresholds
    .map((t) => `${LEVELS[t.level]} ${percent(t.ratio)}`)
    .join(" · ")
}

export function ProgramsView({ at }: { at?: string }) {
  const { text: adminText } = useAdminUI()
  const { adminQueries } = useAdminServices()
  const { data, isPending, error } = useQuery({
    ...adminQueries.disputePrograms(at),
    // The widget raises no toast.
    meta: undefined,
  })
  const pspKey = usePSPKeys(true)

  if (isPending)
    return (
      <div className="flex h-full flex-col justify-center gap-2">
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-2/3" />
      </div>
    )
  if (error)
    return (
      <div
        className="flex h-full items-center justify-center text-center text-xs text-destructive"
        role="alert"
      >
        {errorText(error).text}
      </div>
    )
  if (data.data.length === 0)
    return (
      <div className="flex h-full min-h-16 items-center justify-center text-sm text-muted-foreground">
        {adminText("no card PSP")}
      </div>
    )
  return (
    <div className="h-full overflow-auto">
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="text-xs">{adminText("PSP")}</TableHead>
            <TableHead className="text-xs">{adminText("Program")}</TableHead>
            <TableHead className="text-right text-xs">
              {adminText("So far")}
            </TableHead>
            <TableHead className="text-right text-xs">
              {adminText("Projected")}
            </TableHead>
            <TableHead className="text-xs">{adminText("Level")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.data.map((s) => (
            <TableRow key={`${s.psp_id}:${s.program}`} title={levels(s)}>
              <TableCell className="text-xs">{pspKey(s.psp_id)}</TableCell>
              <TableCell className="text-xs">{PROGRAMS[s.program]}</TableCell>
              <TableCell className="text-right text-xs tabular-nums">
                {percent(s.ratio)}{" "}
                <span className="text-muted-foreground">
                  ({s.count}/{s.denominator})
                </span>
              </TableCell>
              <TableCell className="text-right text-xs tabular-nums">
                {percent(s.projected_ratio)}
              </TableCell>
              <TableCell className="text-xs">
                <Badge
                  variant={
                    s.level === "none"
                      ? "outline"
                      : s.level === "warning"
                        ? "secondary"
                        : "destructive"
                  }
                  className={cn(s.level === "none" && "text-muted-foreground")}
                >
                  {adminText(LEVELS[s.level])}
                </Badge>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
