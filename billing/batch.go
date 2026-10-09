package billing

// MaxBatchItems bounds one batch write: EnsureCustomers,
// CreateProductAccess, AcknowledgeHostEvents and MarkNotificationsRead. A
// longer batch is refused with invalid_param, never split.
const MaxBatchItems = 100
