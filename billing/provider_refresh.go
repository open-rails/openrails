package billing

// ProviderRefresh reports a requested provider refresh for the bound
// merchant. Status is "queued" or "already_running" (an in-flight refresh
// absorbs the request and is started now).
type ProviderRefresh struct {
	Status string `json:"status"`
	JobID  int64  `json:"job_id,string"`
}
