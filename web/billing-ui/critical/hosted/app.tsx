import { createRoot } from "react-dom/client"
import {
  EmbeddedPayment,
  type EmbeddedPaymentCompletion,
} from "@openrails/billing-ui"

declare global {
  interface Window {
    hostedFixture: { urls: string[]; orders: string[]; attacker: string }
    completions: EmbeddedPaymentCompletion[]
    messageSeen: number
    showOrder: (index: number) => void
    showURL: (url: string) => void
  }
}
window.hostedFixture = await (await fetch("/__fixture/config")).json()
window.completions = []
window.messageSeen = 0
window.addEventListener("message", () => {
  window.messageSeen++
})
const root = createRoot(document.getElementById("app")!)
window.showURL = (url) =>
  root.render(
    <EmbeddedPayment
      url={url}
      fallbacks={{ ccbill: { for: ["stripe"], afterFailures: 2 } }}
      onComplete={(result) => {
        window.completions.push(result)
      }}
    />
  )
window.showOrder = (index) => window.showURL(window.hostedFixture.urls[index])
window.showOrder(0)
document.body.dataset.ready = "true"
