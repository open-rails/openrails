package subscriptions

import "time"

const (
	DunningInterval    = 72 * time.Hour
	MaxDunningFailures = 5
)
