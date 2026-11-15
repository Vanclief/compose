package relational_test

import (
	"bytes"
	"context"
	"database/sql/driver"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/postgres"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pgtest"
	"github.com/vanclief/ez"
)

const (
	waitingLine = "Waiting for another instance to finish creating tables or extensions"
	gaveUpLine  = "Did not get the create lock in time, creating tables or extensions without it"
)

// createLockDBs - Creates a database with createLockDatabase and connects
// instances handles to it, each with its own pools, standing in for instances
// of an app booting together
func createLockDBs(t *testing.T, instances, maxOpenConns int) []*relational.DB {
	cfg := createLockDatabase(t, maxOpenConns)

	dbs := make([]*relational.DB, instances)
	for i := range dbs {
		dbs[i] = connectCreateLock(t, cfg)
	}

	return dbs
}

// createLockDatabase - Creates a database with pgtest and returns the config
// to connect to it, whose main pools allow maxOpenConns connections. The
// database is dropped when the test ends. It skips when PostgreSQL is
// unreachable, unless COMPOSE_TEST_POSTGRES is set.
func createLockDatabase(t *testing.T, maxOpenConns int) postgres.ConnectionConfig {
	cfg := postgres.ConnectionConfig{
		Username:     "postgres",
		Host:         "localhost:5432",
		Database:     "compose_create_lock",
		MaxOpenConns: maxOpenConns,
		MaxIdleConns: maxOpenConns,
	}

	created, err := pgtest.Create(cfg)
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}

		t.Skipf("PostgreSQL is not available: %v", err)
	}
	t.Cleanup(func() {
		err := pgtest.Drop(created)
		assert.NoError(t, err)
	})

	return created
}

// connectCreateLock - Connects a handle with its own pools to the database of
// cfg, closed when the test ends
func connectCreateLock(t *testing.T, cfg postgres.ConnectionConfig) *relational.DB {
	db, err := postgres.ConnectToDatabase(&cfg)
	require.NoError(t, err)

	// Registered after the drop, so it runs before it
	t.Cleanup(func() {
		db.Close() // nolint:errcheck // The test is over
	})

	return db
}

// captureLogs - Sends the global logger to a buffer until the test ends
func captureLogs(t *testing.T) *bytes.Buffer {
	previous := log.Logger
	t.Cleanup(func() {
		log.Logger = previous
	})

	var buf bytes.Buffer
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))

	return &buf
}

// within - Runs call and fails the test when it has not returned after limit
func within(t *testing.T, limit time.Duration, call func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- call()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("The call has not returned after %s", limit)
		return nil
	}
}

// createLockHeld - Whether any session holds the create lock on db's database
func createLockHeld(t *testing.T, db *relational.DB) bool {
	var held bool
	err := db.NewRaw(`SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
		AND (classid::bigint << 32 | objid::bigint) = ?
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	)`, relational.CreateLockKey).Scan(context.Background(), &held)
	require.NoError(t, err)

	return held
}

// holdCreateLock - Takes the create lock on a session of db's main pool, as
// another instance creating its tables would, until the test ends
func holdCreateLock(t *testing.T, db *relational.DB) {
	ctx := context.Background()

	conn, err := db.DB.DB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		conn.Raw(func(any) error { // nolint:errcheck // Closes the session, and the lock with it
			return driver.ErrBadConn
		})
		conn.Close() // nolint:errcheck // Raw already closed it
	})

	_, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", relational.CreateLockKey)
	require.NoError(t, err)
}

// tableExists - Whether db's database has the table name
func tableExists(t *testing.T, db *relational.DB, name string) bool {
	var exists bool
	err := db.NewRaw("SELECT to_regclass(?) IS NOT NULL", name).Scan(context.Background(), &exists)
	require.NoError(t, err)

	return exists
}

type lockFunctioned struct {
	bun.BaseModel `bun:"table:lock_functioned"`

	ID int64 `bun:",pk,autoincrement"`
}

var _ bun.AfterCreateTableHook = (*lockFunctioned)(nil)

// AfterCreateTable replaces a function on every call through query.DB(). The
// function body reads the table, which PostgreSQL checks when it creates the
// function, so the hook fails unless its connection sees the table.
func (*lockFunctioned) AfterCreateTable(ctx context.Context, query *bun.CreateTableQuery) error {
	_, err := query.DB().ExecContext(ctx, "CREATE OR REPLACE FUNCTION lock_functioned_count() RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM lock_functioned'")
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

type lockGranted struct {
	bun.BaseModel `bun:"table:lock_granted"`

	ID int64 `bun:",pk,autoincrement"`
}

var _ bun.AfterCreateTableHook = (*lockGranted)(nil)

// AfterCreateTable grants a privilege on the table on every call through
// query.DB(), which rewrites the table's catalog row
func (*lockGranted) AfterCreateTable(ctx context.Context, query *bun.CreateTableQuery) error {
	_, err := query.DB().ExecContext(ctx, "GRANT SELECT ON lock_granted TO PUBLIC")
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// TestCreateTablesLockSerializesHooks: instances booting together all run a
// model's hook that rewrites a catalog row, which fails with "tuple
// concurrently updated" when two run it at the same moment
func TestCreateTablesLockSerializesHooks(t *testing.T) {
	const rounds = 30
	const instances = 4
	ctx := context.Background()
	dbs := createLockDBs(t, instances, 2)

	models := map[string]interface{}{
		"CREATE OR REPLACE FUNCTION": (*lockFunctioned)(nil),
		"GRANT":                      (*lockGranted)(nil),
	}
	for name, model := range models {
		t.Run(name, func(t *testing.T) {
			failedRounds := 0
			failedCalls := 0
			for round := 0; round < rounds; round++ {
				_, err := dbs[0].NewDropTable().Model(model).IfExists().Exec(ctx)
				require.NoError(t, err)
				_, err = dbs[0].ExecContext(ctx, "DROP FUNCTION IF EXISTS lock_functioned_count()")
				require.NoError(t, err)

				start := make(chan struct{})
				errs := make(chan error, instances)
				var wg sync.WaitGroup
				for _, db := range dbs {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						errs <- db.CreateTables([]interface{}{model})
					}()
				}
				close(start)
				wg.Wait()
				close(errs)

				failed := 0
				for err := range errs {
					if err == nil {
						continue
					}
					if failed == 0 {
						t.Errorf("round %d: %v", round, err)
					}
					failed++
				}
				if failed > 0 {
					failedRounds++
					failedCalls += failed
				}
			}

			t.Logf("%d of %d rounds failed, %d of %d calls", failedRounds, rounds, failedCalls, rounds*instances)
			assert.Zero(t, failedRounds)
		})
	}
}

type lockBroken struct {
	bun.BaseModel `bun:"table:lock_broken"`

	ID   int64  `bun:",pk,autoincrement"`
	Kind string `bun:"type:no_such_type"`
}

type lockPanicking struct {
	bun.BaseModel `bun:"table:lock_panicking"`

	ID int64 `bun:",pk,autoincrement"`
}

var _ bun.AfterCreateTableHook = (*lockPanicking)(nil)

func (*lockPanicking) AfterCreateTable(context.Context, *bun.CreateTableQuery) error {
	panic("create lock test")
}

// TestCreateTablesReleasesLock: the lock is released when CreateTables
// succeeds, fails or panics in a hook, so another instance does not wait
func TestCreateTablesReleasesLock(t *testing.T) {
	dbs := createLockDBs(t, 2, 2)
	first := dbs[0]
	second := dbs[1]
	logs := captureLogs(t)

	cases := map[string]func(t *testing.T){
		"success": func(t *testing.T) {
			err := first.CreateTables([]interface{}{(*lockFunctioned)(nil)})
			require.NoError(t, err)
		},
		"model error": func(t *testing.T) {
			err := first.CreateTables([]interface{}{(*lockBroken)(nil)})
			require.Error(t, err)
		},
		"hook panic": func(t *testing.T) {
			assert.PanicsWithValue(t, "create lock test", func() {
				first.CreateTables([]interface{}{(*lockPanicking)(nil)}) // nolint:errcheck // It panics before returning
			})
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			logs.Reset()

			call(t)
			assert.False(t, createLockHeld(t, first), "the lock is still held")

			err := within(t, 10*time.Second, func() error {
				return second.CreateTables([]interface{}{(*lockGranted)(nil)})
			})
			require.NoError(t, err)
			assert.NotContains(t, logs.String(), waitingLine)
		})
	}
}

// TestCreateTablesLockWaitIsBounded: an instance that finds the lock held
// for longer than createLockWait creates its tables without it
func TestCreateTablesLockWaitIsBounded(t *testing.T) {
	dbs := createLockDBs(t, 2, 2)
	relational.SetCreateLockWait(t, 500*time.Millisecond)
	logs := captureLogs(t)
	holdCreateLock(t, dbs[1])

	start := time.Now()
	err := within(t, 10*time.Second, func() error {
		return dbs[0].CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond)
	assert.True(t, tableExists(t, dbs[0], "lock_functioned"))

	assert.Contains(t, logs.String(), waitingLine)
	assert.Contains(t, logs.String(), gaveUpLine)

	// CreateExtensions waits for the same lock, in a wait of its own.
	start = time.Now()
	err = within(t, 10*time.Second, func() error {
		return dbs[0].CreateExtensions([]string{"plpgsql"})
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond)
	assert.Equal(t, 2, strings.Count(logs.String(), gaveUpLine))
}

// reentrantDB is the handle lockReentrant's hook calls CreateTables on. bun
// runs hooks on the model's zero value, so the model cannot carry it.
var reentrantDB *relational.DB

type lockReentrant struct {
	bun.BaseModel `bun:"table:lock_reentrant"`

	ID int64 `bun:",pk,autoincrement"`
}

var _ bun.AfterCreateTableHook = (*lockReentrant)(nil)

// AfterCreateTable creates another model with CreateTables on the same
// handle, while the outer call holds the lock
func (*lockReentrant) AfterCreateTable(context.Context, *bun.CreateTableQuery) error {
	return reentrantDB.CreateTables([]interface{}{(*lockFunctioned)(nil)})
}

// TestCreateTablesFromHookDoesNotDeadlock: a hook that calls CreateTables
// waits createLockWait for the lock its own outer call holds, then goes on
func TestCreateTablesFromHookDoesNotDeadlock(t *testing.T) {
	db := createLockDBs(t, 1, 2)[0]
	relational.SetCreateLockWait(t, 500*time.Millisecond)
	logs := captureLogs(t)

	reentrantDB = db
	t.Cleanup(func() {
		reentrantDB = nil
	})

	err := within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockReentrant)(nil)})
	})
	require.NoError(t, err)
	assert.True(t, tableExists(t, db, "lock_reentrant"))
	assert.True(t, tableExists(t, db, "lock_functioned"))
	assert.Contains(t, logs.String(), gaveUpLine)
	assert.False(t, createLockHeld(t, db))
}

// TestCreateTablesOnOneConnectionPool: without a migration pool, a main pool
// of one connection has none to spare for the lock, so the call does not take
// it instead of waiting forever for a connection
func TestCreateTablesOnOneConnectionPool(t *testing.T) {
	connected := createLockDBs(t, 1, 1)[0]
	db := &relational.DB{DB: connected.DB}

	err := within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)
	assert.True(t, tableExists(t, db, "lock_functioned"))

	err = within(t, 10*time.Second, func() error {
		return db.CreateExtensions([]string{"plpgsql"})
	})
	require.NoError(t, err)

	// The same pool given as the migration pool is still one connection.
	db = &relational.DB{DB: connected.DB, MigrationDB: connected.DB}
	err = within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)

	// So is another handle on the same pool.
	db = &relational.DB{DB: connected.DB, MigrationDB: bun.NewDB(connected.DB.DB, connected.DB.Dialect())}
	err = within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)
}

// A main pool of two connections with one of them busy, as it is when a
// migration that runs on the main pool calls CreateTables, has none to spare
// for the lock either.
func TestCreateTablesOnBusyLimitedPool(t *testing.T) {
	connected := createLockDBs(t, 1, 2)[0]
	db := &relational.DB{DB: connected.DB}

	tx, err := db.DB.Begin()
	require.NoError(t, err)
	defer tx.Rollback() // nolint:errcheck // The test is over

	err = within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)
	assert.True(t, tableExists(t, db, "lock_functioned"))
	assert.False(t, createLockHeld(t, db))
}

// A main pool without a connection limit has a connection to spare, so a
// relational.DB built without MigrationDB still takes the lock out of it.
func TestCreateTablesOnUnlimitedPoolTakesLock(t *testing.T) {
	dbs := createLockDBs(t, 2, 0)
	db := &relational.DB{DB: dbs[0].DB}
	relational.SetCreateLockWait(t, 500*time.Millisecond)
	logs := captureLogs(t)
	holdCreateLock(t, dbs[1])

	err := within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)
	assert.Contains(t, logs.String(), waitingLine)
	assert.Contains(t, logs.String(), gaveUpLine)
}

// A lock that cannot be taken is not a reason for the call to fail: the
// migration pool is closed here, and the tables are created on the main one.
func TestCreateTablesWithoutLockWhenItCannotBeTaken(t *testing.T) {
	db := createLockDBs(t, 1, 2)[0]
	logs := captureLogs(t)

	require.NotNil(t, db.MigrationDB)
	err := db.MigrationDB.Close()
	require.NoError(t, err)

	err = within(t, 10*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFunctioned)(nil)})
	})
	require.NoError(t, err)
	assert.True(t, tableExists(t, db, "lock_functioned"))
	assert.Contains(t, logs.String(), "Could not take the create lock")

	err = within(t, 10*time.Second, func() error {
		return db.CreateExtensions([]string{"plpgsql"})
	})
	require.NoError(t, err)
}

// freezingProxy - A TCP proxy that drops what either side sends once frozen,
// standing in for a server or network that stops answering
type freezingProxy struct {
	listener net.Listener
	frozen   atomic.Bool
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool
	conns  []net.Conn
}

// startFreezingProxy - Starts a freezingProxy to target, closed with all its
// connections when the test ends
func startFreezingProxy(t *testing.T, target string) *freezingProxy {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	proxy := &freezingProxy{listener: listener}
	proxy.wg.Add(1)
	go proxy.accept(target)
	t.Cleanup(proxy.close)

	return proxy
}

func (p *freezingProxy) accept(target string) {
	defer p.wg.Done()

	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}

		server, err := net.Dial("tcp", target)
		if err != nil {
			client.Close() // nolint:errcheck // The client sees the dial failed
			continue
		}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			client.Close() // nolint:errcheck // The proxy is closing
			server.Close() // nolint:errcheck // The proxy is closing
			return
		}
		p.conns = append(p.conns, client, server)
		p.wg.Add(2)
		p.mu.Unlock()

		go p.pipe(server, client)
		go p.pipe(client, server)
	}
}

// pipe - Copies what src sends to dst, dropping it while the proxy is frozen,
// and closes both when either fails
func (p *freezingProxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	defer dst.Close() // nolint:errcheck // Ends the other direction too
	defer src.Close() // nolint:errcheck // Ends the other direction too

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}

		if p.frozen.Load() {
			continue
		}

		_, err = dst.Write(buf[:n])
		if err != nil {
			return
		}
	}
}

func (p *freezingProxy) close() {
	p.listener.Close() // nolint:errcheck // Ends accept

	p.mu.Lock()
	p.closed = true
	for _, conn := range p.conns {
		conn.Close() // nolint:errcheck // Ends its pipes
	}
	p.mu.Unlock()

	p.wg.Wait()
}

// hookProxy is the proxy lockFreezing's hook freezes. bun runs hooks on the
// model's zero value, so the model cannot carry it.
var hookProxy *freezingProxy

type lockFreezing struct {
	bun.BaseModel `bun:"table:lock_freezing"`

	ID int64 `bun:",pk,autoincrement"`
}

var _ bun.AfterCreateTableHook = (*lockFreezing)(nil)

// AfterCreateTable freezes the proxy while the call holds the lock
func (*lockFreezing) AfterCreateTable(context.Context, *bun.CreateTableQuery) error {
	hookProxy.frozen.Store(true)
	return nil
}

// TestCreateTablesUnlockIsBounded: when the server stops answering while
// CreateTables holds the lock, the release, on a migration pool without a
// read timeout, gives up after the unlock timeout and closes the lock's
// connection instead of hanging the call
func TestCreateTablesUnlockIsBounded(t *testing.T) {
	cfg := createLockDatabase(t, 2)
	direct := connectCreateLock(t, cfg)
	relational.SetAdvisoryUnlockTimeout(t, 500*time.Millisecond)
	logs := captureLogs(t)

	hookProxy = startFreezingProxy(t, cfg.Host)
	t.Cleanup(func() {
		hookProxy = nil
	})
	cfg.Host = hookProxy.listener.Addr().String()
	db := connectCreateLock(t, cfg)

	err := within(t, 5*time.Second, func() error {
		return db.CreateTables([]interface{}{(*lockFreezing)(nil)})
	})
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "Could not release an advisory lock, closing its connection")

	// The proxy passes the close on, so the server ends the lock's session
	deadline := time.Now().Add(5 * time.Second)
	for createLockHeld(t, direct) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	assert.False(t, createLockHeld(t, direct), "the lock is still held")
}
