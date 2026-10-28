import type { PSPImportMapping } from "#orck/client/generated/wire"

// A mappings file is CSV with the header mapping,key,customer; an empty
// customer deletes the mapping. Rows are checked as the server checks them,
// so a refusal names its line before anything is sent.

export interface MappingRow extends PSPImportMapping {
  line: number
}

export const MAPPING_CHUNK = 100

const HEADER = ["mapping", "key", "customer"]
const MAPPING_NAME = /^[a-z][a-z0-9_.-]{0,63}$/
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const encoder = new TextEncoder()

// csvRecords splits CSV text into records of fields, with each record's
// first line number. Quoted fields may hold commas, quotes ("") and newlines.
function csvRecords(text: string): { line: number; fields: string[] }[] {
  const out: { line: number; fields: string[] }[] = []
  let fields: string[] = []
  let field = ""
  let quoted = false
  let line = 1
  let start = 1
  const end = () => {
    fields.push(field)
    if (fields.length > 1 || fields[0] !== "") out.push({ line: start, fields })
    fields = []
    field = ""
  }
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (quoted) {
      if (c === '"' && text[i + 1] === '"') {
        field += '"'
        i++
      } else if (c === '"') quoted = false
      else {
        if (c === "\n") line++
        field += c
      }
    } else if (c === '"' && field === "") quoted = true
    else if (c === ",") {
      fields.push(field)
      field = ""
    } else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++
      end()
      line++
      start = line
    } else field += c
  }
  if (field !== "" || fields.length) end()
  return out
}

export function parseMappingCSV(text: string): {
  rows: MappingRow[]
  errors: string[]
} {
  const records = csvRecords(text.replace(/^\uFEFF/, ""))
  const errors: string[] = []
  const header = records.shift()
  if (
    !header ||
    header.fields.map((f) => f.trim().toLowerCase()).join(",") !==
      HEADER.join(",")
  )
    return { rows: [], errors: ["The first line must be mapping,key,customer"] }
  const rows: MappingRow[] = []
  const seen = new Map<string, number>()
  for (const { line, fields } of records) {
    if (fields.length !== 3) {
      errors.push(`Line ${line}: expected 3 fields, found ${fields.length}`)
      continue
    }
    const mapping = fields[0].trim()
    const key = fields[1].trim()
    const customer = fields[2].trim()
    if (!MAPPING_NAME.test(mapping))
      errors.push(`Line ${line}: "${mapping}" is not a mapping name`)
    const bytes = encoder.encode(key).length
    if (bytes < 1 || bytes > 256)
      errors.push(`Line ${line}: the key must be 1 to 256 bytes`)
    if (customer && !UUID.test(customer))
      errors.push(`Line ${line}: "${customer}" is not a customer id`)
    const id = `${mapping}\u001f${key}`
    const first = seen.get(id)
    if (first !== undefined)
      errors.push(`Line ${line} repeats the key of line ${first}`)
    else seen.set(id, line)
    rows.push({ line, mapping, key, customer: customer.toLowerCase() || null })
  }
  if (!rows.length && !errors.length) errors.push("The file has no mappings")
  return { rows, errors }
}

export function chunks<T>(rows: T[], size = MAPPING_CHUNK): T[][] {
  const out: T[][] = []
  for (let i = 0; i < rows.length; i += size) out.push(rows.slice(i, i + size))
  return out
}
