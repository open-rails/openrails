package postgresmigrations

import (
	"embed"
)

//go:embed *.sql
var FS embed.FS
