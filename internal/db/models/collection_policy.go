package models

// CollectionPolicy names the immutable owner of a subscription's collection.
// It belongs to the obligation, never to a replaceable payment instrument.
type CollectionPolicy string

const (
	CollectionPolicyProvider        CollectionPolicy = "provider"
	CollectionPolicyProviderDunning CollectionPolicy = "provider_dunning"
	CollectionPolicyEngine          CollectionPolicy = "engine"
)

func (p CollectionPolicy) Valid() bool {
	return p == CollectionPolicyProvider || p == CollectionPolicyProviderDunning || p == CollectionPolicyEngine
}
