// Solana Pay transfer request: the official SDK owns QR rendering while the
// checkout owns lifecycle copy and the same-device wallet link.
import * as React from "react"

import { buttonVariants } from "#orck/components/ui/button"
import { cn } from "cn"

// Keep the official renderer's full quiet zone. Cropping it makes the tile
// denser but materially reduces scan reliability on lower-quality cameras.
const QR_SIZE = 208

export function SolanaBody({
  amountLabel,
  payload,
  statusLine,
}: {
  amountLabel: string
  payload?: string
  statusLine: string
}) {
  const qrRef = React.useRef<HTMLDivElement | null>(null)
  const [qrState, setQrState] = React.useState<"loading" | "ready" | "failed">(
    "loading"
  )

  React.useEffect(() => {
    const container = qrRef.current
    if (!container || !payload) return

    let cancelled = false
    container.replaceChildren()
    setQrState("loading")
    void import("@solana/pay")
      .then(({ createQR }) => {
        if (cancelled) return
        createQR(payload, QR_SIZE, "#ffffff", "#18181b").append(container)
        setQrState("ready")
      })
      .catch(() => {
        if (!cancelled) setQrState("failed")
      })
    return () => {
      cancelled = true
      container.replaceChildren()
    }
  }, [payload])

  return (
    <div className="grid justify-items-center gap-3 py-1">
      <div className="grid place-items-center rounded-xl bg-white p-1 shadow-sm ring-1 ring-black/10">
        {payload ? (
          <div
            role="img"
            aria-label="Solana Pay QR code"
            className="relative overflow-hidden rounded-lg"
            style={{ width: QR_SIZE, height: QR_SIZE }}
          >
            <div ref={qrRef} className="absolute inset-0" />
            {qrState === "loading" ? (
              <div
                aria-hidden
                className="absolute inset-0 animate-pulse rounded-lg bg-zinc-100"
              />
            ) : null}
            {qrState === "failed" ? (
              <div className="absolute inset-0 grid place-items-center text-center text-xs text-zinc-600">
                QR code unavailable
              </div>
            ) : null}
          </div>
        ) : (
          <div
            aria-hidden
            className="animate-pulse rounded-lg bg-zinc-100"
            style={{ width: QR_SIZE, height: QR_SIZE }}
          />
        )}
      </div>
      <div className="text-[13px] font-medium text-balance">
        Scan with a Solana wallet
      </div>
      <div
        className="text-center text-[12.5px] text-pretty text-muted-foreground tabular-nums"
        aria-live="polite"
      >
        {amountLabel} · {statusLine}
      </div>
      {payload ? (
        <a
          href={payload}
          className={cn(
            buttonVariants({ variant: "secondary" }),
            "h-9 w-full max-w-56"
          )}
        >
          Open in wallet
        </a>
      ) : null}
    </div>
  )
}
