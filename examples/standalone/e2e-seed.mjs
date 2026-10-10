// Signs up the browser tests' users as a person does (compose.e2e.yaml):
// register, then the code AuthKit emailed, read from Mailpit's API, proves
// the address. A user who exists already is kept. AuthKit spaces sign-ups
// from one address a minute apart, so this takes a few minutes.
const app = process.env.E2E_BASE_URL
const password = process.env.E2E_PASSWORD
const json = { "Content-Type": "application/json", Origin: app }
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

for (const name of process.env.E2E_USERS.split(",")) {
  const email = `${name}@example.com`
  let registered, error
  for (;;) {
    registered = await fetch(`${app}/api/v1/register`, {
      method: "POST",
      headers: json,
      body: JSON.stringify({ identifier: email, username: name, password }),
    })
    error = registered.ok ? null : (await registered.json()).error
    if (registered.status !== 429) break
    const wait = error?.metadata?.retry_after_seconds ?? 60
    console.log(`waiting ${wait}s to sign up ${email}`)
    await sleep(wait * 1000 + 500)
  }
  if (error?.code === "username_in_use") continue // signed up already
  if (error) throw new Error(`register ${email}: ${registered.status} ${error.code}`)
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
  const verified = await fetch(`${app}/api/v1/verify/confirm`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ identifier: email, code }),
  })
  if (!verified.ok) throw new Error(`verify ${email}: ${verified.status} ${await verified.text()}`)
  console.log(`signed up ${email}`)
}
