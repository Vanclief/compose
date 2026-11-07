package postgres

import (
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/compose/drivers/databases/relational/internal/jsonfmt"
)

// jsonDialect is bun's Postgres dialect with jsonfmt.AppendJSON in place of
// bun's JSON formatter. Embedding the pointer keeps every other method of
// pgdialect.Dialect, so bun sees the Postgres name and features.
type jsonDialect struct {
	*pgdialect.Dialect
}

func (jsonDialect) AppendJSON(b, jsonb []byte) []byte {
	return jsonfmt.AppendJSON(b, jsonb)
}

// NewDialect returns the bun dialect ConnectToDatabase uses: pgdialect.New()
// with a JSON formatter that stores the text \u0000 correctly on bun v1.3.0
// (https://github.com/uptrace/bun/issues/1472). Pass it to bun.NewDB when
// building a *bun.DB without ConnectToDatabase.
func NewDialect() schema.Dialect {
	return jsonDialect{pgdialect.New()}
}
