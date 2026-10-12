// A deploy rotates chunk hashes, so an open tab's lazy chunk is gone: only a
// document reload fetches the new index.html.
const CHUNK_ERRORS = [
  /dynamically imported module/i,
  /failed to load module script/i,
  /importing a module script failed/i,
  /loading (css )?chunk \S+ failed/i,
  /unable to preload css/i,
]

export function isChunkLoadError(error: unknown): boolean {
  const e = error as { name?: string; message?: string } | null
  if (e?.name === "ChunkLoadError") return true
  return CHUNK_ERRORS.some((pattern) => pattern.test(e?.message ?? ""))
}
