package session

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

// bunStore keeps sessions in one table of a bun database, PostgreSQL or
// SQLite.
type bunStore struct {
	db    *bun.DB
	table string
}

// bunRecord is the row shape. The table name is set per query from the
// table given to NewBunStore. previous_hash is NULL until the first
// rotation, first_used_at until the current token is first used and
// expires_at when MaxTTL is Never.
//
// grug: SQLite has no time type, so bun writes times as UTC text
// ("2006-01-02 15:04:05.999999+00:00") and reads them back as UTC. That text
// sorts in time order, which is what the expires_at and last_used_at
// comparisons of DeleteExpired rely on there. Every other time rule runs in
// Go in the Manager.
type bunRecord struct {
	bun.BaseModel `bun:"table:sessions"`

	ID           string    `bun:"id,pk,notnull,type:text"`
	Subject      string    `bun:"subject,notnull,type:text"`
	TokenHash    []byte    `bun:"token_hash,notnull"`
	PreviousHash []byte    `bun:"previous_hash,nullzero"`
	CreatedAt    time.Time `bun:"created_at,notnull"`
	LastUsedAt   time.Time `bun:"last_used_at,notnull"`
	RotatedAt    time.Time `bun:"rotated_at,notnull"`
	FirstUsedAt  time.Time `bun:"first_used_at,nullzero"`
	ExpiresAt    time.Time `bun:"expires_at,nullzero"`
	Revision     int64     `bun:"revision,notnull,default:0"`
}

// mutableColumns are the columns Update writes.
var mutableColumns = []string{"token_hash", "previous_hash", "first_used_at", "last_used_at", "rotated_at", "expires_at", "revision"}

func toRow(r Record) *bunRecord {
	return &bunRecord{
		ID:           r.ID,
		Subject:      r.Subject,
		TokenHash:    r.TokenHash,
		PreviousHash: r.PreviousHash,
		CreatedAt:    r.CreatedAt,
		LastUsedAt:   r.LastUsedAt,
		RotatedAt:    r.RotatedAt,
		FirstUsedAt:  r.FirstUsedAt,
		ExpiresAt:    r.ExpiresAt,
		Revision:     r.Revision,
	}
}

func (row *bunRecord) record() Record {
	return Record{
		Session: Session{
			ID:          row.ID,
			Subject:     row.Subject,
			CreatedAt:   row.CreatedAt,
			LastUsedAt:  row.LastUsedAt,
			RotatedAt:   row.RotatedAt,
			FirstUsedAt: row.FirstUsedAt,
			ExpiresAt:   row.ExpiresAt,
		},
		TokenHash:    row.TokenHash,
		PreviousHash: row.PreviousHash,
		Revision:     row.Revision,
	}
}

// NewBunStore returns a Store backed by one table with the columns id,
// subject, token_hash, previous_hash, created_at, last_used_at, rotated_at,
// first_used_at, expires_at and revision. Call CreateTable with the same
// table at startup to create it. Expired rows stay in the table until
// Resolve meets them, Sweep deletes them or the subject is revoked. Nothing
// calls Sweep automatically; run it periodically as the Manager.Sweep doc
// shows.
func NewBunStore(db *bun.DB, table string) Store {
	return &bunStore{db: db, table: table}
}

// CreateTable creates the table of NewBunStore when it does not exist, with a
// unique index on token_hash and plain indexes on previous_hash, subject,
// expires_at and last_used_at, named after the table with "_token_hash",
// "_previous_hash", "_subject", "_expires_at" and "_last_used_at" suffixes in
// the schema of the table. The last two serve Sweep.
func CreateTable(ctx context.Context, db *bun.DB, table string) error {
	_, err := db.NewCreateTable().
		Model((*bunRecord)(nil)).
		ModelTableExpr("?", bun.Ident(table)).
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: creating the "+table+" table failed", err)
	}
	err = createIndex(ctx, db, table, "token_hash", true)
	if err != nil {
		return ez.Wrap(err)
	}
	err = createIndex(ctx, db, table, "previous_hash", false)
	if err != nil {
		return ez.Wrap(err)
	}
	err = createIndex(ctx, db, table, "subject", false)
	if err != nil {
		return ez.Wrap(err)
	}
	err = createIndex(ctx, db, table, "expires_at", false)
	if err != nil {
		return ez.Wrap(err)
	}
	err = createIndex(ctx, db, table, "last_used_at", false)
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// createIndex creates the index "<table>_<column>" in the schema of table.
// For "db.schema.table", PostgreSQL takes an unqualified index name and the
// qualified table:
//
//	CREATE INDEX IF NOT EXISTS "table_subject" ON "db"."schema"."table" ("subject")
//
// SQLite takes the schema on the index name and a bare table:
//
//	CREATE INDEX IF NOT EXISTS "schema"."table_subject" ON "table" ("subject")
func createIndex(ctx context.Context, db *bun.DB, table, column string, unique bool) error {
	index := table + "_" + column
	on := table
	qualifier, name := indexParts(table)
	if qualifier != "" {
		index = name + "_" + column
		if db.Dialect().Name() == dialect.SQLite {
			index = qualifier + "." + index
			on = name
		}
	}
	query := db.NewCreateIndex().
		IfNotExists().
		Index(index).
		TableExpr("?", bun.Ident(on)).
		Column(column)
	if unique {
		query = query.Unique()
	}
	_, err := query.Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: creating the "+column+" index of the "+table+" table failed", err)
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

func (s *bunStore) Insert(ctx context.Context, r Record) error {
	_, err := s.db.NewInsert().
		Model(toRow(r)).
		ModelTableExpr("?", bun.Ident(s.table)).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: saving the session failed", err)
	}
	return nil
}

func (s *bunStore) FindByHash(ctx context.Context, hash []byte) (Record, error) {
	row := new(bunRecord)
	err := s.db.NewSelect().
		Model(row).
		ModelTableExpr("? AS ?TableAlias", bun.Ident(s.table)).
		Where("token_hash = ? OR previous_hash = ?", hash, hash).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ez.New(ez.ENOTFOUND, "session: no session for token", nil)
	}
	if err != nil {
		return Record{}, ez.New(ez.EINTERNAL, "session: reading the session failed", err)
	}
	return row.record(), nil
}

// Update sets every mutable column, revision included as r.Revision+1, on
// the row still at r.Revision, in one UPDATE.
func (s *bunStore) Update(ctx context.Context, r Record) error {
	row := toRow(r)
	row.Revision = r.Revision + 1
	result, err := s.db.NewUpdate().
		Model(row).
		ModelTableExpr("?", bun.Ident(s.table)).
		Column(mutableColumns...).
		Where("id = ?", r.ID).
		Where("revision = ?", r.Revision).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: updating the session failed", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: counting updated rows failed", err)
	}
	if count == 0 {
		return ez.New(ez.ENOTFOUND, "session: no session to update", nil)
	}
	return nil
}

func (s *bunStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().
		TableExpr("?", bun.Ident(s.table)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: deleting the session failed", err)
	}
	return nil
}

// DeleteRevision deletes the row still at revision, in one DELETE.
func (s *bunStore) DeleteRevision(ctx context.Context, id string, revision int64) error {
	result, err := s.db.NewDelete().
		TableExpr("?", bun.Ident(s.table)).
		Where("id = ?", id).
		Where("revision = ?", revision).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: deleting the session failed", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: counting deleted rows failed", err)
	}
	if count == 0 {
		return ez.New(ez.ENOTFOUND, "session: no session to delete", nil)
	}
	return nil
}

func (s *bunStore) DeleteBySubject(ctx context.Context, subject string) error {
	_, err := s.db.NewDelete().
		TableExpr("?", bun.Ident(s.table)).
		Where("subject = ?", subject).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "session: deleting the sessions of the subject failed", err)
	}
	return nil
}

// DeleteExpired deletes the expired rows in one DELETE. A NULL expires_at
// (MaxTTL Never) never matches: the comparison is NULL, not true.
func (s *bunStore) DeleteExpired(ctx context.Context, now, idleBefore time.Time) (int64, error) {
	query := s.db.NewDelete().
		TableExpr("?", bun.Ident(s.table)).
		Where("expires_at IS NOT NULL AND expires_at <= ?", now)
	if !idleBefore.IsZero() {
		query = query.WhereOr("last_used_at <= ?", idleBefore)
	}
	result, err := query.Exec(ctx)
	if err != nil {
		return 0, ez.New(ez.EINTERNAL, "session: sweeping expired sessions failed", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, ez.New(ez.EINTERNAL, "session: counting swept sessions failed", err)
	}
	return count, nil
}
