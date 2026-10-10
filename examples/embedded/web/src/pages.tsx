import { useEffect, useState } from "react"
import { useNavigate, useParams } from "react-router-dom"
import { SignInDialog } from "@openrails/auth-ui"
import { createAuthClient } from "@openrails/auth-ui/client"
import { useAuth } from "@openrails/auth-ui/react"
import { createBillingClient } from "@openrails/billing-ui/client"
import { BillingUiProvider, CheckoutModal } from "@openrails/billing-ui"
import "@openrails/billing-ui/styles.css"

export const auth = createAuthClient() // AuthKit's browser client, at /api/v1: authFetch attaches the signed-in user's token
const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

const courses: Record<string, { title: string; product: string }> = {
  "css-101": { title: "Intro to CSS", product: "course-101" },
  "tailwind-102": { title: "Intro to Tailwind", product: "course-102" },
}

export function CoursePage() {
  const { course = "" } = useParams()
  const lessons = useGated<{ lessons: string[] }>(`/api/courses/${course}`)?.lessons
  return <ol>{lessons?.map((lesson) => <li key={lesson}>{lesson}</li>)}</ol>
}

export function MembersQAPage() {
  const questions = useGated<{ questions: string[] }>("/api/members/qa")?.questions
  return <ul>{questions?.map((question) => <li key={question}>{question}</li>)}</ul>
}

// useGated reads gated content as the signed-in user; without access the
// server answers 402 with where to buy it, and the page goes there.
function useGated<T>(api: string): T | undefined {
  const navigate = useNavigate()
  const [body, setBody] = useState<T>()
  useEffect(() => {
    auth.authFetch(api).then(async (res) => {
      if (res.status === 402) navigate((await res.json()).buy, { replace: true }) // no access: go buy it
      else if (res.ok) setBody(await res.json())
    })
  }, [api, navigate])
  return body
}

export function BuyCoursePage() {
  const { course = "" } = useParams()
  const navigate = useNavigate()
  const item = courses[course]
  if (!item) return <p>Not found</p>
  const paid = () => navigate(`/courses/${course}`) // the gate now lets them in
  return (
    <BillingUiProvider appearance={{ theme: "auto" }}>
      <h1>{item.title}</h1>
      <p>Three lessons, yours to keep.</p>
      <Buy product={item.product} price="purchase" label="Buy for $4.99" onPaid={paid} />
      <Buy product={item.product} price="rent" label="Rent for 3 days, $1.99" onPaid={paid} />
      <Buy product="course-bundle" price="purchase" label="Both courses for $8.99" onPaid={paid} />
    </BillingUiProvider>
  )
}

export function JoinPage() {
  const navigate = useNavigate()
  const paid = () => navigate("/members/qa")
  return (
    <BillingUiProvider appearance={{ theme: "auto" }}>
      <h1>Channel membership</h1>
      <Buy product="channel-membership" price="monthly" label="$10 every 30 days" onPaid={paid} />
      <Buy product="channel-membership" price="yearly" label="$99 every 365 days" onPaid={paid} />
    </BillingUiProvider>
  )
}

function Buy({ product, price, label, onPaid }: { product: string; price: string; label: string; onPaid: () => void }) {
  const { signedIn } = useAuth()
  const [signingIn, setSigningIn] = useState(false)
  const [session, setSession] = useState<string>()
  async function start() {
    if (!signedIn) return setSigningIn(true) // anyone sees the page; buying needs an account
    // OpenRails prices the offer from the catalog; card entry happens in the processor's iframe.
    setSession((await billing.createCheckoutSession({ productKey: product, priceKey: price })).id)
  }
  return (
    <>
      <button onClick={start}>{label}</button>
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
      {session && (
        <CheckoutModal
          open
          onOpenChange={(open) => !open && setSession(undefined)}
          source={billing.checkoutSource(session)}
          onComplete={(result) => result.status === "succeeded" && onPaid()}
        />
      )}
    </>
  )
}
