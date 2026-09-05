package merchant

import "context"

// NameAuthority supplies the host's authoritative merchant-group directory.
// Bound billing rows never substitute their cached names for these reads.
// Implementations must use one namespace for both operations and return only
// current owners or still-valid forwards; a miss is an error, never a fallback.
type NameAuthority interface {
	ResolveGroup(context.Context, string) (groupID, canonicalName string, err error)
	GroupName(context.Context, string) (canonicalName string, err error)
}

// GroupName is a canonical name returned by the authoritative group directory.
type GroupName struct{ ID, Name string }

// GroupNameSearch is the optional canonical-name search capability. Point
// resolution includes valid aliases; canonical search deliberately does not.
type GroupNameSearch interface {
	SearchGroups(context.Context, string, string, string, int) ([]GroupName, error)
}
