// Tile grids take their columns from the content and the space, not from
// device sizes, and step only to counts that divide their tiles evenly, so a
// row never ends in a lone tile. Steps are measured against the page
// container: a stat tile needs 10rem (its widest uppercase label word, 140px
// with padding, and a four-digit amount), a fact tile 12rem (a date and time
// on one line), with 0.75rem gaps.

/** Stat tiles in fours: 1, 2 (from 20.75rem), 4 (from 42.25rem). */
export const STAT_TILES_4 =
  "grid gap-3 grid-cols-[minmax(0,1fr)] @min-[20.75rem]/page:grid-cols-2 @min-[42.25rem]/page:grid-cols-4"

/** Stat tiles in sixes: 1, 2, 3 (from 31.5rem), 6 (from 64.25rem). */
export const STAT_TILES_6 =
  "grid gap-3 grid-cols-[minmax(0,1fr)] @min-[20.75rem]/page:grid-cols-2 @min-[31.5rem]/page:grid-cols-3 @min-[64.25rem]/page:grid-cols-6"

/** Fact tiles: 1, 2 (from 24.75rem), 4 (from 50.25rem). */
export const FACT_TILES =
  "grid gap-3 grid-cols-[minmax(0,1fr)] @min-[24.75rem]/page:grid-cols-2 @min-[50.25rem]/page:grid-cols-4"
