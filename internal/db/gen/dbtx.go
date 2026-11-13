package gen

// DBTX is the handle q runs on: a job queued with q's work joins its
// transaction (db.InsertRiverJobWith).
func (q *Queries) DBTX() DBTX { return q.db }
