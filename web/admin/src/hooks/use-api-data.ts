import * as React from "react"

// useApiData: tiny fetch-on-deps hook; no cache layer (admin console reads
// are cheap and always fresh).
export function useApiData<T>(
  fn: () => Promise<T>,
  deps: React.DependencyList
) {
  const [data, setData] = React.useState<T | null>(null)
  const [loading, setLoading] = React.useState(true)
  const [error, setError] = React.useState<unknown>(null)
  const [nonce, setNonce] = React.useState(0)

  React.useEffect(() => {
    let cancelled = false
    // eslint-disable-next-line react-hooks/set-state-in-effect -- deliberate: flag a fresh in-flight load on dep change
    setLoading(true)
    fn().then(
      (res) => {
        if (!cancelled) {
          setData(res)
          setError(null)
          setLoading(false)
        }
      },
      (err) => {
        if (!cancelled) {
          setError(err)
          setLoading(false)
        }
      }
    )
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  const reload = React.useCallback(() => setNonce((n) => n + 1), [])
  return { data, loading, error, reload }
}
