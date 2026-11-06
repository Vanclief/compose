package pagination

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/vanclief/ez"
)

// fakeConn is a database/sql connection that records every query and answers
// count queries with count and any other query with rows, or every query with
// err when it is set.
type fakeConn struct {
	queries []string
	count   int64
	columns []string
	rows    [][]driver.Value
	err     error
}

type fakeConnector struct {
	conn *fakeConn
}

type fakeDriver struct{}

type fakeRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

// newFakeDB returns a bun DB on a fakeConn. Queries render with the postgres
// dialect and never reach a server.
func newFakeDB(conn *fakeConn) *bun.DB {
	return bun.NewDB(sql.OpenDB(&fakeConnector{conn: conn}), pgdialect.New())
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return c.conn, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return fakeDriver{}
}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, ez.New(ez.EINTERNAL, "fake driver opens through its connector", nil)
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, ez.New(ez.EINTERNAL, "fake connection does not prepare statements", nil)
}

func (c *fakeConn) Close() error {
	return nil
}

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, ez.New(ez.EINTERNAL, "fake connection does not begin transactions", nil)
}

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.queries = append(c.queries, query)

	if c.err != nil {
		return nil, c.err
	}

	if strings.HasPrefix(query, "SELECT count(*)") {
		return &fakeRows{columns: []string{"count"}, rows: [][]driver.Value{{c.count}}}, nil
	}

	return &fakeRows{columns: c.columns, rows: c.rows}, nil
}

func (r *fakeRows) Columns() []string {
	return r.columns
}

func (r *fakeRows) Close() error {
	return nil
}

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}

	copy(dest, r.rows[r.next])
	r.next++

	return nil
}
