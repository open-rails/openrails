package billing

// ErrEventStoreRequired is the refusal of a feature that needs the event
// store on a deployment without one (Deps.ClickHouse unset): HTTP 409
// event_store_required. It is definite, never ErrUnreachable.
var ErrEventStoreRequired error = newCodedError(CodeEventStoreRequired, ErrConflict)
