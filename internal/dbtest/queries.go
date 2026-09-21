package dbtest

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Queries runs authored sqlc queries against the shared billing fixture using
// the production schema rewriter. The handle retains its connection, transaction
// and merchant scope. Custom-schema tests use their configured DB.Gen instead.
func Queries(handle gen.DBTX) *gen.Queries {
	return gen.New(db.RewriteDBTX(handle, config.DefaultSchema))
}
