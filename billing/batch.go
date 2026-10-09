package billing

// MaxBatchItems bounds one batch: the items or ids of a batch write or lookup,
// and the ids a List names. A longer batch is refused, never split.
const MaxBatchItems = 100
