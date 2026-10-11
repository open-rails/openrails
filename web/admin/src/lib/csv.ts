// CSV export: RFC 4180 quoting, downloaded as a file.

export function csvEscape(v: unknown): string {
  const s = v == null ? "" : String(v)
  return /[",\n\r]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s
}

export function toCsv(header: string[], rows: unknown[][]): string {
  return [header, ...rows].map((row) => row.map(csvEscape).join(",")).join("\n")
}

export function downloadCsv(
  filename: string,
  header: string[],
  rows: unknown[][]
) {
  const url = URL.createObjectURL(
    new Blob([toCsv(header, rows)], { type: "text/csv;charset=utf-8" })
  )
  const a = document.createElement("a")
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
