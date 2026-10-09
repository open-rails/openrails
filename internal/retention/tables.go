package retention

// Class is how a table's rows leave it.
type Class string

const (
	// Permanent rows are never pruned: money, grants, invoices, receipts and
	// the records they depend on.
	Permanent Class = "permanent"
	// PartitionedMonthly tables drop whole monthly partitions by the calendar.
	PartitionedMonthly Class = "partitioned"
	// Rows tables delete rows past a period, in bounded batches.
	Rows Class = "rows"
	// State tables hold what currently exists (configuration, entities,
	// cursors): one row per thing, gone only when the thing is.
	State Class = "state"
)

// Table is one table's retention. Rule is the sentence its table comment
// carries after "Retention: "; State tables carry none.
type Table struct {
	Class Class
	Rule  string
}

const permanent = "permanent, never pruned."

// Tables classifies every table of the baseline. A table without an entry
// fails TestEveryTableHasARetentionClass.
var Tables = map[string]Table{
	// Money, grants, invoices and receipts.
	"ledger_accounts":                     {Permanent, permanent},
	"ledger_transfers":                    {Permanent, permanent},
	"grants":                              {Permanent, permanent},
	"payments":                            {Permanent, permanent},
	"invoices":                            {Permanent, permanent},
	"invoice_collection_cadence":          {Permanent, permanent},
	"invoice_items":                       {Permanent, permanent},
	"invoice_payments":                    {Permanent, permanent},
	"operation_authorizations":            {Permanent, permanent},
	"operation_authorization_extensions":  {Permanent, permanent},
	"cost_qualifications":                 {Permanent, permanent},
	"metered_rating_watermarks":           {Permanent, permanent},
	"solana_pay_receipts":                 {Permanent, "permanent for credited and review receipts; an ignored receipt goes with its settled reference."},
	"catalog_applications":                {Permanent, permanent},
	"catalog_restore_receipts":            {Permanent, permanent},
	"merchant_configuration_applications": {Permanent, permanent},
	"credential_publications":             {Permanent, permanent},
	"product_archive_operations":          {Permanent, permanent},
	"destructive_run_before_images":       {Permanent, permanent},
	"custody_migrations":                  {Permanent, permanent},
	"payment_method_updates":              {Permanent, permanent},
	"price_key_movements":                 {Permanent, permanent},
	"reprice_batches":                     {Permanent, permanent},
	"subscription_reprices":               {Permanent, permanent},
	"account_updater_batches":             {Permanent, permanent},
	"admission_denials_hourly":            {Permanent, permanent},
	"webhook_health_daily":                {Permanent, permanent},

	"usage_events":         {PartitionedMonthly, "monthly partitions on occurred_at, dropped 24 months after the month's usage was invoiced."},
	"admission_operations": {PartitionedMonthly, "monthly partitions on admitted_at, dropped once older than the longest spend window plus 30 days."},

	"subscription_status_transitions": {Rows, "rows are deleted 25 months (761 days) after occurred_at, by the cleanup job only."},
	"provider_intents":                {Rows, "finished intents that only instructed a provider (cancel, update, archive, vault, token, account updater) are deleted 25 months (761 days) after they last changed; an intent that moved or refused money, enrolled a membership or erased a card is permanent."},
	"provider_mutation_logs":          {Rows, "rows are deleted 25 months (761 days) after created_at."},
	"cost_observations":               {Rows, "rows are deleted 90 days after their operation was settled or released, by the cleanup job only."},
	"reconciliation_findings":         {Rows, "resolved findings are deleted 12 months (366 days) after they were resolved and last seen."},
	"maintenance_runs":                {Rows, "reconciliation runs no finding refers to are deleted 12 months (366 days) after they started, by the cleanup job only; every other kind is permanent."},
	"checkout_attempts":               {Rows, "attempts that expired without reaching a provider are deleted 90 days after expires_at; every other attempt is permanent."},
	"checkout_sessions":               {Rows, "rows are deleted at purge_at, 24 hours after the session expired."},
	"notifications":                   {Rows, "rows are deleted 90 days after created_at once read, 180 days if never read."},
	"webhook_events":                  {Rows, "completed events are deleted 90 days after completed_at."},
	"host_outbox":                     {Rows, "delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted."},
	"payment_attempts":                {Rows, "rows are deleted 25 months (761 days) after attempted_at."},
	"rebill_cycles":                   {Rows, "rows are deleted 25 months (761 days) after due_at, once their attempts are gone."},
	"nmi_history_months":              {Rows, "rows are deleted 25 months (761 days) after their month."},
	"idempotency_keys":                {Rows, "rows are deleted at expires_at."},
	"card_attempt_failures":           {Rows, "buckets are deleted once older than the longest card-abuse window."},
	"solana_pay_references":           {Rows, "settled references are deleted after their 7-day watch window."},

	"merchants":                   {Class: State},
	"merchant_slug_aliases":       {Class: State},
	"merchant_api_host_claims":    {Class: State},
	"destructive_action_switch":   {Class: State},
	"merchant_destructive_policy": {Class: State},
	"worker_state":                {Class: State},
	"merchant_configurations":     {Class: State},
	"merchant_deks":               {Class: State},
	"merchant_secrets":            {Class: State},
	"merchant_webhooks":           {Class: State},
	"dashboard_configs":           {Class: State},
	"federated_grants":            {Class: State},
	"customers":                   {Class: State},
	"customer_invoice_profiles":   {Class: State},
	"customer_delinquency":        {Class: State},
	"custodians":                  {Class: State},
	"psps":                        {Class: State},
	"psp_customers":               {Class: State},
	"products":                    {Permanent, permanent},
	"prices":                      {Permanent, permanent},
	"price_psp_bindings":          {Class: State},
	"catalog_meters":              {Class: State},
	"catalog_rate_cards":          {Class: State},
	"payment_methods":             {Class: State},
	"subscriptions":               {Class: State},
	"subscription_verifications":  {Class: State},
	"solana_subscriptions":        {Class: State},
	"entitlements":                {Class: State},
	"billing_policies":            {Class: State},
	"billing_policy_bindings":     {Class: State},
	"money_settings":              {Class: State},
	"invoker_spend_limits":        {Class: State},
	"psp_refresh_watermarks":      {Class: State},
	"webhook_health":              {Class: State},
	"nmi_bulk_checkpoints":        {Class: State},
	"nmi_history_reads":           {Class: State},
	"reconciliation_state":        {Class: State},
}
