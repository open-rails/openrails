package destructive

// Limits are how many destructive actions staff may take in a rolling hour
// and day before a hold stops them: each actor's, each merchant's and the
// deployment's. The merchant and deployment limits are floors that rise by
// one per 400 (hour) and 100 (day) active customers. Contact erasures keep
// their own budget per merchant, whose floors rise with its directory; past
// it they wait rather than fail.
//
// Counted actions are refunds, cancellations, payment-method removal, access
// and credit revocation, write-offs, and changes that archive or remove an
// existing catalog object's billable, access or routing terms; creating an
// object spends nothing, nor do replays and proven no-ops. A batch counts its
// targets. The actor is the verified issuer and invoker. An in-process call
// that names no invoker is the host's own work: audited, it spends nothing
// and no hold stops it. A breach answers 429 and opens a hold that persists
// until a person clears it (openrails destructive-holds): a legitimate bulk
// job asks staff to clear it.
type Limits struct {
	ActorPerHour, ActorPerDay           int64
	MerchantPerHour, MerchantPerDay     int64
	DeploymentPerHour, DeploymentPerDay int64
	ErasurePerHour, ErasurePerDay       int64
}

// DefaultLimits are the limits every deployment runs with.
var DefaultLimits = Limits{
	ActorPerHour: 5, ActorPerDay: 15,
	MerchantPerHour: 15, MerchantPerDay: 25,
	DeploymentPerHour: 50, DeploymentPerDay: 100,
	ErasurePerHour: 100, ErasurePerDay: 1000,
}

// SetLimits replaces a's limits: a test seam, so a scenario trips or clears
// a hold in a few actions. Production runs with DefaultLimits.
func (a *Admissions) SetLimits(l Limits) {
	if a != nil {
		a.policy.Store(&l)
	}
}
