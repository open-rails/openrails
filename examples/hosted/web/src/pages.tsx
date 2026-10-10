import { useEffect, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { SignInDialog } from "@openrails/auth-ui"
import { useAuth } from "@openrails/auth-ui/react"
import { BuyButton, formatAmount, Offers } from "@openrails/billing-ui"

import { auth, billing } from "./clients"

// A course as the app's API lists it: the host's own, priced by OpenRails. A
// members-only one is sold with the membership's prices.
type Course = {
  slug: string
  title: string
  members_only: boolean
  owned: boolean
  product_key: string
  prices: { key: string; amount: string; currency: string; terms: string }[]
}

// StorePage lists the courses a page at a time: Watch for those the user
// owns, a BuyButton per price for the rest (the membership's, for a
// members-only video).
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
          {course.members_only && <p>Members only</p>}
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
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
    </>
  )
}

// CoursePage plays the course, if it's theirs.
export function CoursePage() {
  const { course = "" } = useParams()
  const navigate = useNavigate()
  const [videoURL, setVideoURL] = useState<string>()
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    auth.authFetch(`/api/courses/${course}`).then(async (res) => {
      switch (res.status) {
        case 200: // theirs: the server answers a short-lived video URL
          setVideoURL((await res.json()).video_url)
          break
        case 402: // not theirs yet: the server says where to buy it
          navigate((await res.json()).buy, { replace: true })
          break
        default: // unknown course, or the server is unavailable
          setFailed(true)
      }
    })
  }, [course, navigate])
  if (failed) return <p>Couldn't load this course.</p>
  if (!videoURL) return <p>Loading…</p>
  return <video src={videoURL} controls />
}

// CourseBuyPage asks the course what unlocks it and offers everything on sale
// that grants any of those keys: the course, the bundle, the membership. Paid,
// the buyer goes back to the course, which now lets them in.
export function CourseBuyPage() {
  const { course = "" } = useParams()
  const navigate = useNavigate()
  const { signedIn } = useAuth()
  const [signingIn, setSigningIn] = useState(false)
  const [unlock, setUnlock] = useState<string[]>()
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    auth.authFetch(`/api/courses/${course}`).then(async (res) => {
      switch (res.status) {
        case 200: // already theirs: back to it
          navigate(`/courses/${course}`, { replace: true })
          break
        case 402: // not theirs yet: the server names the keys that unlock it
          setUnlock((await res.json()).unlock)
          break
        default: // unknown course, or the server is unavailable
          setFailed(true)
      }
    })
  }, [course, navigate])
  if (failed) return <p>Couldn't load this course.</p>
  if (!unlock) return <p>Loading…</p>
  return (
    <>
      <Offers
        entitlements={unlock}
        signedIn={signedIn}
        onSignInRequired={() => setSigningIn(true)}
        onPaid={() => navigate(`/courses/${course}`)}
      />
      <SignInDialog open={signingIn} onOpenChange={setSigningIn} />
    </>
  )
}
