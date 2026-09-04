package riverjobs

// preferSweepError retains one failure while independent work continues. A
// structural failure takes priority so an earlier transient PostgreSQL error
// cannot hide schema drift from the existing River classifier.
func preferSweepError(current, next error) error {
	if current == nil {
		return next
	}
	if _, structural := classifyStructural(next); structural {
		if _, retained := classifyStructural(current); !retained {
			return next
		}
	}
	return current
}
