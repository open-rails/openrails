package subscriptions

type GetNotificationsFilters struct {
	UserID    string `form:"user_id"`
	EventType string `form:"event_type"`
	Seen      *bool  `form:"seen"`
}
