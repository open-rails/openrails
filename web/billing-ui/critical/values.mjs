import assert from "node:assert/strict"
import { addAmounts, isAmount } from "@openrails/billing-ui"
import { amountToDecimal, decimalToAmount } from "@openrails/billing-ui/client"

// Independent literal vectors at the shipped public entrypoints. No DOM,
// browser, Intl formatting matrix, or copied private arithmetic algorithm.
for (const [amount, scale, decimal] of [
  ["29970000", 6, "29.97"],
  ["9007199254740993", 6, "9007199254.740993"],
  ["9223372036854775807", 6, "9223372036854.775807"],
  ["-9223372036854775808", 6, "-9223372036854.775808"],
  ["1", 9, "0.000000001"],
  ["1", 18, "0.000000000000000001"],
]) {
  assert.equal(amountToDecimal(amount, scale), decimal)
  assert.equal(decimalToAmount(decimal, scale), amount)
}
assert.equal(addAmounts("9990000", "9990000", "9990000"), "29970000")
assert.equal(addAmounts("9223372036854775807", "-9223372036854775808"), "-1")
assert.equal(addAmounts("9223372036854775807", "1"), null)
assert.equal(addAmounts("-9223372036854775808", "-1"), null)
for (const value of [
  "1.0000001",
  "9223372036854.775808",
  "-9223372036854.775809",
  "1e3",
  " 9.99",
  "NaN",
])
  assert.equal(decimalToAmount(value, 6), null)
for (const scale of [-1, 1.5, 19])
  assert.equal(decimalToAmount("1", scale), null)
assert.equal(isAmount(9007199254740993), false)
assert.equal(isAmount("9223372036854775808"), false)
assert.equal(isAmount("-9223372036854775808"), true)
console.log(
  "Built public monetary helpers preserve exact int64 arithmetic, native precision and refusal boundaries."
)
