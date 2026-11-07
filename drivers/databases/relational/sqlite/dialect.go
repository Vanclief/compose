package sqlite

import (
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/compose/drivers/databases/relational/internal/jsonfmt"
)

// jsonDialect is bun's SQLite dialect with jsonfmt.AppendJSON in place of
// bun's JSON formatter. Embedding the pointer keeps every other method of
// sqlitedialect.Dialect, so bun sees the SQLite name and features.
type jsonDialect struct {
	*sqlitedialect.Dialect
}

func (jsonDialect) AppendJSON(b, jsonb []byte) []byte {
	return jsonfmt.AppendJSON(b, jsonb)
}

// NewDialect returns the bun dialect ConnectToDatabase and
// ConnectToMemoryDatabase use: sqlitedialect.New() with a JSON formatter that
// stores the text \u0000 correctly on bun v1.3.0
// (https://github.com/uptrace/bun/issues/1472). Pass it to bun.NewDB when
// building a *bun.DB without them.
func NewDialect() schema.Dialect {
	return jsonDialect{sqlitedialect.New()}
}
