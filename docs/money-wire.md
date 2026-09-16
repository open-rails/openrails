# Money on the HTTP wire

Go uses signed int64 native currency units. Deposit, ledger receipt, balance,
checkout-session, capture, admission, usage-report, wasted-spend and invoice DTOs encode monetary values as decimal
JSON strings. Invoice movement maps use the same representation. A JavaScript
consumer must use BigInt or an exact decimal library for arithmetic; converting
to Number before parsing loses values above 2^53.

Invoice responses include unit_decimals. For USD, "1234567" with scale 6 means
1.234567 USD. JPY uses its declared scale, not an assumed cents/micros scale.
Counts and timestamps are not money: counts remain numbers, and invoice/receipt
timestamps use RFC3339 with fractional seconds. Missing optional timestamps are
omitted; explicit nullable receipt fields use null.

The admin invoice UI submits decimal strings and formats them with BigInt,
including values beyond JavaScript's safe integer range. No compatibility
parser accepts numeric money on these finalized routes during the pre-v1 cut.
Remaining unconverted endpoints are tracked in #983 and are not frozen yet.
