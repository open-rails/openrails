import type { PSPImport } from "#orck/client/generated/wire"

// ImportProgress is where an import's job is, read from the fields it keeps:
// the catalog and roster first, then history windows, then linking; Apply
// lands while its imported count grows.
export type ImportProgress =
  | { phase: "reading" }
  | { phase: "history"; through: string; percent?: number }
  | { phase: "linking" }
  | { phase: "landing" }
  | { phase: "pulled" }
  | { phase: "applied" }
  | { phase: "failed" }

export function importProgress(imp: PSPImport): ImportProgress {
  switch (imp.status) {
    case "applying":
      return { phase: "landing" }
    case "pulled":
    case "applied":
    case "failed":
      return { phase: imp.status }
  }
  if (!imp.pulled_at) return { phase: "reading" }
  const pulled = Date.parse(imp.pulled_at)
  const through = imp.history_through ? Date.parse(imp.history_through) : NaN
  if (!(through < pulled)) return { phase: "linking" }
  const since = imp.history_since ? Date.parse(imp.history_since) : NaN
  const percent =
    since < pulled
      ? Math.max(
          0,
          Math.min(
            100,
            Math.floor(((through - since) / (pulled - since)) * 100)
          )
        )
      : undefined
  return { phase: "history", through: imp.history_through!, percent }
}

// recordCount is how many staged records are in state, of kind when set.
export function recordCount(
  imp: PSPImport,
  state: PSPImport["counts"][number]["state"],
  kind?: PSPImport["counts"][number]["kind"]
) {
  return imp.counts
    .filter((c) => c.state === state && (!kind || c.kind === kind))
    .reduce((sum, c) => sum + c.records, 0)
}
