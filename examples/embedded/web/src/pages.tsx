import { useEffect, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { SignInDialog } from "@openrails/auth-ui"
import { createAuthClient } from "@openrails/auth-ui/client"
import { useAuth } from "@openrails/auth-ui/react"
import { createBillingClient, formatAmount } from "@openrails/billing-ui/client"
import { BuyButton, Offers } from "@openrails/billing-ui"

export const auth = createAuthClient() // AuthKit's browser client, at /api/v1: authFetch attaches the signed-in user's token
export const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })

// A course as the app's API lists it: the host's own, priced by OpenRails.
type Course = {
  slug: string
  title: string
  product_key: string
  owned: boolean
  prices: { key: string; amount: string; currency: string; terms: string }[]
}

// StorePage lists the courses a page at a time: Watch for those the user
// owns, a BuyButton per price for the rest.
export function StorePage() {
  const navigate = useNavigate()
  const { signedIn } = useAuth()
  const [signingIn, setSigningIn] = useState(false)
  const [cursor, setCursor] = useState("0")
  const [pages, setPages] = useState<{ data: Course[]; next_cursor: string | null }[]>([])
  useEffect(() => {
    let current = true
    auth
      .authFetch(`/api/courses?cursor=${cursor}`)
      .then((res) => res.json())
      .then((page) => current && setPages((pages) => [...pages, page]))
    return () => {
      current = false
    }
  }, [cursor])
  const next = pages.at(-1)?.next_cursor
  return (
    <>
      {pages.flatMap((page) => page.data).map((course) => (
        <section key={course.slug}>
          <h2>{course.title}</h2>
          {course.owned ? (
            <Link to={`/courses/${course.slug}`}>Watch</Link>
          ) : (
            course.prices.map((price) => (
              <BuyButton
                key={price.key}
                product={course.product_key}
                price={price.key}
                label={`${formatAmount(price.amount, price.currency, billing.currencies[price.currency])} ${price.terms}`}
                signedIn={signedIn}
                onSignInRequired={() => setSigningIn(true)}
                onPaid={() => navigate(`/courses/${course.slug}`)}
              />
            ))
          )}
        </section>
      ))}
      {next && <button onClick={() => setCursor(next)}>Load more</button>}
      <Link to="/members/qa">Members-only Q&A</Link>
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
    </>
  )
}

// CoursePage plays the course: the API answers its signed video URL, or 402
// and where to buy it.
export function CoursePage() {
  const { course = "" } = useParams()
  const navigate = useNavigate()
  const [videoURL, setVideoURL] = useState<string>()
  useEffect(() => {
    auth.authFetch(`/api/courses/${course}`).then(async (res) => {
      const body = await res.json()
      if (res.status === 402) navigate(body.buy, { replace: true }) // no access: go buy it
      else if (res.ok) setVideoURL(body.video_url)
    })
  }, [course, navigate])
  return videoURL ? <video src={videoURL} controls /> : null
}

export function MembersQAPage() {
  const navigate = useNavigate()
  const [questions, setQuestions] = useState<string[]>()
  useEffect(() => {
    auth.authFetch("/api/members/qa").then(async (res) => {
      const body = await res.json()
      if (res.status === 402) navigate(body.buy, { replace: true })
      else if (res.ok) setQuestions(body.questions)
    })
  }, [navigate])
  return <ul>{questions?.map((question) => <li key={question}>{question}</li>)}</ul>
}

export function CourseBuyPage() {
  const { course = "" } = useParams()
  return <BuyPage api={`/api/courses/${course}`} back={`/courses/${course}`} />
}

export function JoinPage() {
  return <BuyPage api="/api/members/qa" back="/members/qa" />
}

// BuyPage asks api what unlocks it (its 402 names the entitlement) and offers
// everything on sale that grants it: the course, a bundle, a membership.
// Paid, the buyer goes back, which now lets them in.
function BuyPage({ api, back }: { api: string; back: string }) {
  const navigate = useNavigate()
  const { signedIn } = useAuth()
  const [signingIn, setSigningIn] = useState(false)
  const [entitlement, setEntitlement] = useState<string>()
  useEffect(() => {
    auth.authFetch(api).then(async (res) => {
      if (res.status === 402) setEntitlement((await res.json()).entitlement)
      else if (res.ok) navigate(back, { replace: true }) // already theirs
    })
  }, [api, back, navigate])
  if (!entitlement) return null
  return (
    <>
      <Offers entitlement={entitlement} signedIn={signedIn} onSignInRequired={() => setSigningIn(true)} onPaid={() => navigate(back)} />
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
    </>
  )
}
