package oauth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/vanclief/ez"
)

// bunStore keeps records in one table of a bun database, PostgreSQL or
// SQLite. Expired rows are never read; Sweep deletes them.
type bunStore struct {
	db    *bun.DB
	table string
}

// bunRecord is the row shape. The table name is set per query from the
// table given to NewBunStore.
//
// grug: SQLite has no time type, so bun writes expires_at as UTC text
// ("2006-01-02 15:04:05.999999+00:00"). That text sorts in time order, which
// is what the expires_at comparisons rely on there.
type bunRecord struct {
	bun.BaseModel `bun:"table:oauth_records"`

	Kind      string    `bun:"kind,pk,notnull,type:text"`
	ID        string    `bun:"id,pk,notnull,type:text"`
	Value     []byte    `bun:"value,notnull"`
	ExpiresAt time.Time `bun:"expires_at,notnull"`
}

// NewBunStore returns a Store backed by one table with the columns kind, id,
// value and expires_at, primary key (kind, id). Call CreateTable with the
// same table at startup to create it. Take uses a single DELETE ... RETURNING
// statement so a record is redeemed at most once. Expired rows stay in the
// table until Sweep deletes them; nothing calls Sweep automatically, so run
// it periodically, for example from a ticker goroutine as the Sweep doc shows.
func NewBunStore(db *bun.DB, table string) Store {
	return &bunStore{db: db, table: table}
}

// CreateTable creates the table of NewBunStore and its index on expires_at,
// named after the table with an "_expires_at" suffix in the schema of the
// table, when they do not exist.
func CreateTable(ctx context.Context, db *bun.DB, table string) error {
	_, err := db.NewCreateTable().
		Model((*bunRecord)(nil)).
		ModelTableExpr("?", bun.Ident(table)).
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: creating the "+table+" table failed", err)
	}
	// The index lives in the schema of the table. For "db.schema.table",
	// PostgreSQL takes an unqualified index name and the qualified table:
	//   CREATE INDEX IF NOT EXISTS "table_expires_at" ON "db"."schema"."table" ("expires_at")
	// SQLite takes the schema on the index name and a bare table:
	//   CREATE INDEX IF NOT EXISTS "schema"."table_expires_at" ON "table" ("expires_at")
	index := table + "_expires_at"
	on := table
	qualifier, name := indexParts(table)
	if qualifier != "" {
		index = name + "_expires_at"
		if db.Dialect().Name() == dialect.SQLite {
			index = qualifier + "." + index
			on = name
		}
	}
	_, err = db.NewCreateIndex().
		IfNotExists().
		Index(index).
		TableExpr("?", bun.Ident(on)).
		Column("expires_at").
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: creating the expires_at index of the "+table+" table failed", err)
	}
	return nil
}

// indexParts splits table at its last dot into the qualifier and the bare
// table name. An unqualified table has an empty qualifier.
func indexParts(table string) (qualifier, name string) {
	i := strings.LastIndex(table, ".")
	if i < 0 {
		return "", table
	}
	return table[:i], table[i+1:]
}

// Sweep deletes the expired rows of the NewBunStore table and returns how
// many it deleted. Run it periodically, for example from a goroutine started
// with the application:
//
//	go func() {
//		ticker := time.NewTicker(time.Hour)
//		defer ticker.Stop()
//		for {
//			select {
//			case <-ctx.Done():
//				return
//			case <-ticker.C:
//				_, err := oauth.Sweep(ctx, db, "oauth_records")
//				if err != nil {
//					logger.Error("sweeping oauth records failed", "error", err)
//				}
//			}
//		}
//	}()
func Sweep(ctx context.Context, db *bun.DB, table string) (int64, error) {
	result, err := db.NewDelete().
		TableExpr("?", bun.Ident(table)).
		Where("expires_at <= ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return 0, ez.New(ez.EINTERNAL, "oauth: sweeping expired records failed", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, ez.New(ez.EINTERNAL, "oauth: counting swept records failed", err)
	}
	return count, nil
}

func (s *bunStore) Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error {
	// grug: bun writes a nil slice as NULL, which the NOT NULL column rejects.
	if value == nil {
		value = []byte{}
	}
	record := &bunRecord{Kind: kind, ID: id, Value: value, ExpiresAt: expiresAt}
	_, err := s.db.NewInsert().
		Model(record).
		ModelTableExpr("?", bun.Ident(s.table)).
		On("CONFLICT (kind, id) DO UPDATE").
		Set("value = EXCLUDED.value").
		Set("expires_at = EXCLUDED.expires_at").
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: saving the "+kind+" record failed", err)
	}
	return nil
}

func (s *bunStore) Get(ctx context.Context, kind, id string) ([]byte, error) {
	var value []byte
	err := s.db.NewSelect().
		TableExpr("?", bun.Ident(s.table)).
		Column("value").
		Where("kind = ?", kind).
		Where("id = ?", id).
		Where("expires_at > ?", time.Now()).
		Scan(ctx, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ez.New(ez.ENOTFOUND, "oauth: no "+kind+" record", nil)
	}
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "oauth: reading the "+kind+" record failed", err)
	}
	return value, nil
}

func (s *bunStore) Take(ctx context.Context, kind, id string) ([]byte, error) {
	var value []byte
	err := s.db.NewDelete().
		TableExpr("?", bun.Ident(s.table)).
		Where("kind = ?", kind).
		Where("id = ?", id).
		Where("expires_at > ?", time.Now()).
		Returning("value").
		Scan(ctx, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ez.New(ez.ENOTFOUND, "oauth: no "+kind+" record", nil)
	}
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "oauth: taking the "+kind+" record failed", err)
	}
	return value, nil
}

func (s *bunStore) Delete(ctx context.Context, kind, id string) error {
	_, err := s.db.NewDelete().
		TableExpr("?", bun.Ident(s.table)).
		Where("kind = ?", kind).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: deleting the "+kind+" record failed", err)
	}
	return nil
}
