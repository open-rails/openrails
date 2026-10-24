import fs from "node:fs"

const [reportPath, selection] = process.argv.slice(2)
const required = new Set(selection.split(","))
const report = JSON.parse(fs.readFileSync(reportPath, "utf8"))
const methods = {
  "OR-UI-01": [
    "getConfig",
    "getCustomerConfig",
    "getAccount",
    "setCartLine",
    "clearCart",
    "createOrder",
    "getOrder",
    "payOrder",
    "getEntitlements",
    "setLowBalanceLine",
    "getUsage",
  ],
  "OR-UI-02": [
    "addPaymentMethod",
    "listPaymentMethods",
    "confirmPaymentMethod",
    "setDefaultPaymentMethod",
    "confirmOrder",
    "getSubscription",
    "setSubscriptionPaymentMethod",
    "previewSubscriptionChange",
    "changeSubscription",
    "confirmSubscriptionChange",
    "cancelSubscription",
    "resumeSubscription",
    "removePaymentMethod",
  ],
  "OR-UI-03": [
    "createNetworkLinkChallenge",
    "createNetworkLink",
    "reportPayURLFailure",
  ],
  "OR-UI-04": ["getAccount", "setCartLine"],
  "OR-UI-05": ["createOrder", "getOrder"],
}
const seen = new Set(),
  errors = [...(report.errors ?? [])]
function walk(suite) {
  for (const child of suite.suites ?? []) walk(child)
  for (const spec of suite.specs ?? [])
    for (const test of spec.tests ?? []) {
      const ids = spec.title.match(/\bOR-UI-\d{2}\b/g) ?? []
      const id = ids[0]
      if (ids.length !== 1 || !required.has(id) || seen.has(id))
        errors.push(`Unexpected or duplicate case: ${spec.title}`)
      seen.add(id)
      if (
        test.expectedStatus !== "passed" ||
        !test.results?.length ||
        test.results.some((x) => x.status !== "passed")
      )
        errors.push(`Nonpassing case: ${id}`)
      const invoked = new Set()
      for (const result of test.results ?? [])
        for (const attachment of result.attachments ?? []) {
          if (attachment.name !== "client-methods") continue
          const body = attachment.body
            ? Buffer.from(attachment.body, "base64").toString()
            : fs.readFileSync(attachment.path, "utf8")
          for (const method of JSON.parse(body)) invoked.add(method)
        }
      for (const method of methods[id] ?? [])
        if (!invoked.has(method)) errors.push(`${id} did not invoke ${method}`)
    }
}
for (const suite of report.suites ?? []) walk(suite)
for (const id of required)
  if (!seen.has(id) || !methods[id]) errors.push(`Missing/unmapped case: ${id}`)
if (!required.size || errors.length) {
  console.error(errors)
  process.exit(1)
}
console.log(
  `All ${seen.size} selected executable browser cases and their public client methods passed; zero skips.`
)
