// Signs up the browser tests' users as a person does (compose.e2e.yaml):
// register, then the code AuthKit emailed, read from Mailpit's API, proves
// the address with the challenge the registration answered. A user who
// exists already is kept. AuthKit spaces sign-ups from one address a minute
// apart, so this takes a few minutes. Every sign-in step proves a DPoP key
// (RFC 9449), as a browser's does.
const app = process.env.E2E_BASE_URL
const password = process.env.E2E_PASSWORD
const json = { "Content-Type": "application/json", Origin: app }
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

const { subtle } = globalThis.crypto
const key = await subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign"])
const { x, y } = await subtle.exportKey("jwk", key.publicKey)
const b64 = (data) => Buffer.from(data).toString("base64url")
// A single-use proof for method and url (no query).
async function proof(method, url) {
  const header = { typ: "dpop+jwt", alg: "ES256", jwk: { kty: "EC", crv: "P-256", x, y } }
  const claims = { htm: method, htu: url, iat: Math.floor(Date.now() / 1000), jti: crypto.randomUUID() }
  const input = `${b64(JSON.stringify(header))}.${b64(JSON.stringify(claims))}`
  const signature = await subtle.sign({ name: "ECDSA", hash: "SHA-256" }, key.privateKey, new TextEncoder().encode(input))
  return `${input}.${b64(new Uint8Array(signature))}`
}
const signIn = async (path, body) => {
  const url = `${app}/auth/v1${path}`
  return fetch(url, { method: "POST", headers: { ...json, DPoP: await proof("POST", url) }, body: JSON.stringify(body) })
}

for (const name of process.env.E2E_USERS.split(",")) {
  const email = `${name}@example.com`
  let registered, error, challenge
  for (;;) {
    registered = await signIn("/register", { identifier: email, username: name, password })
    const body = await registered.json()
    error = registered.ok ? null : body.error
    challenge = body.challenge
    if (registered.status !== 429) break
    const wait = error?.metadata?.retry_after_seconds ?? 60
    console.log(`waiting ${wait}s to sign up ${email}`)
    await sleep(wait * 1000 + 500)
  }
  if (error?.code === "username_in_use") continue // signed up already
  if (error) throw new Error(`register ${email}: ${registered.status} ${error.code}`)
  if (registered.status !== 202 || !challenge) throw new Error(`register ${email}: ${registered.status} without a verification challenge`)
  let code
  for (let i = 0; i < 100 && !code; i++) {
    const found = await (await fetch(`http://mailpit:8025/api/v1/search?query=${encodeURIComponent(`to:${email}`)}`)).json()
    if (found.messages?.length) {
      const message = await (await fetch(`http://mailpit:8025/api/v1/message/${found.messages[0].ID}`)).json()
      code = message.Text.match(/\b\d{6}\b/)?.[0]
    }
    if (!code) await sleep(200)
  }
  if (!code) throw new Error(`no verification code for ${email}`)
  const verified = await signIn("/verify/confirm", { challenge, code })
  if (!verified.ok) throw new Error(`verify ${email}: ${verified.status} ${await verified.text()}`)
  console.log(`signed up ${email}`)
}
