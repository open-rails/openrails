package models

// CollectionPolicy names the immutable owner of a subscription's collection.
// It belongs to the obligation, never to a replaceable payment instrument.
type CollectionPolicy string

const (
	// CollectionPolicyProvider: the provider charges and retries (Stripe and
	// CCBill mirrors).
	CollectionPolicyProvider CollectionPolicy = "provider"
	// CollectionPolicyNMISchedule: NMI charges on its schedule and never
	// retries; OpenRails dunning is the only retry.
	CollectionPolicyNMISchedule CollectionPolicy = "nmi_schedule"
	// CollectionPolicyEngine: OpenRails charges and retries.
	CollectionPolicyEngine CollectionPolicy = "engine"
)

func (p CollectionPolicy) Valid() bool {
	return p == CollectionPolicyProvider || p == CollectionPolicyNMISchedule || p == CollectionPolicyEngine
}

// ProviderCollectionPolicy is the policy of a provider-scheduled subscription
// on rail: every NMI schedule is dunned by OpenRails.
func ProviderCollectionPolicy(rail string) CollectionPolicy {
	if rail == string(RailNMI) {
		return CollectionPolicyNMISchedule
	}
	return CollectionPolicyProvider
}
