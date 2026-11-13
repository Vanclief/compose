# Scheduler

## Running more than one instance

`components/scheduler` keeps its state in the process. An app that runs several instances, each
with the same `Add` calls, runs every recurring job on every instance at every minute boundary.
To run each boundary on one instance, give every instance's scheduler the same claimer:

```go
// At startup, on every instance
err := claims.CreateTable(ctx, db.DB, "scheduler_claims")
if err != nil {
	return ez.Wrap(err)
}

s, err := scheduler.New(
	scheduler.WithClaimer(claims.NewBunStore(db.DB, "scheduler_claims")),
	scheduler.WithLogger(log),
)
```

`claims` is `components/scheduler/claims`. It works on PostgreSQL only. `scheduler.Claimer` is
the interface, for an app that keeps claims somewhere else. `CreateTable` takes a
transaction-level advisory lock around the create, so instances that boot together against a new
database can all call it. PostgreSQL checks the `CREATE` privilege on the schema before it looks
for the table, so `CreateTable` fails for a role without it even when the table exists: an app
whose runtime role cannot create tables creates the table in a migration (`job_id text PRIMARY
KEY`, `tick timestamptz NOT NULL`, `busy_until timestamptz`) and does not call it.

The store expects the default `read committed` isolation. Under a database or role default of
`repeatable read` or `serializable`, instances that claim one boundary together fail with a
serialization error instead of being refused: no boundary runs twice, but each losing instance
logs a claim error.

A claim is keyed by the job id alone. Two apps, or two Schedulers of one app, that share a table
and use the same job id block each other's runs; give each its own table.

Run every instance in the same time zone. `Every` and `Hourly` compare only the minute of the
hour in local time, so zones whose UTC offsets differ by whole hours (UTC and
America/Mexico_City) agree. Zones whose offsets differ by a fraction of an hour (+5:30, +5:45,
-3:30) select different instants for a schedule that fraction does not fit, such as `Hourly` or
`Every(time.Hour)`, and each instance then wins its own boundaries, so the job runs on both.
`Daily` compares hour and minute in its own location, which is the same on every instance unless
it is `time.Local`.

### What it guarantees

- **One run per boundary.** At a due boundary each instance claims it, and only the instance that
  wins runs the job. A boundary is won once, even by an instance that asks late (it woke late,
  or its clock is behind): a boundary older than or equal to the last one claimed
  for the job is refused.
- **No overlap, within the lease.** While a claimed run is going, later boundaries of the same job
  are refused on every instance, as a busy id skips its boundary in one process. The claim is
  released when the job returns, after a panic or a `Supersede` cancel too, and before a pending
  job of the id runs. During shutdown it is released when the job returns and its release
  finishes within the shutdown timeout, and the process waits for `Start` to return.
- **Catch-up claims once.** After a late wake, the instance claims the latest boundary of the
  caught-up range that the job's schedule matches.

### What it does not

- **A run that outlives its lease can overlap.** The lease is the job timeout (`WithJobTimeout`,
  one hour by default) plus one minute, capped at the largest `time.Duration`, counted from the
  claim, and stops a dead instance's claim from blocking the job forever. The job timeout starts
  later, when the job body is about to run. A run outlives its lease when the claim's answer, the
  lease's answer when a leaser is set, the wait for a `WithLimit` permit and the time the job
  takes to return once its job timeout cancels it add up to more than that
  minute, when the job ignores its context, or when it runs under a `Start` context with a
  deadline, which replaces the job timeout. Once the lease expires another instance can win a
  later boundary.
- **An instance killed mid-run blocks the job until the lease expires.** A crash, or a deploy that
  kills the process without waiting for `Start` to return, never releases the claim, so no
  instance runs the job until the lease has passed: up to an hour and a minute with the default
  job timeout. Set `WithJobTimeout` close to how long the jobs really take.
- **On-demand runs are not claimed.** `RunOnce`, `Supersede` and `RunNext` run on the instance
  they are called on, as before. That instance skips a boundary that falls while the on-demand
  run is going, but another instance can claim it, so the on-demand run and that recurring run
  can overlap: "a due boundary is skipped while the id is busy" holds per instance. A `Leaser`
  (below) stops that overlap.
- **`WithLimit` stays per process.** The instance that wins a boundary can be one with no free
  permit. The job then waits there, holding the claim, and later boundaries are refused on every
  instance, even idle ones.
- **Instances without the claimer still run everything.** During the rolling deploy that adds
  `WithClaimer`, instances on the earlier build run every boundary, next to the instance that
  wins the claim, until the rollout finishes.
- **A boundary is skipped when the store does not answer.** A claim that fails is logged at error
  level and that instance does not run the job at that boundary. Missing a boundary is safer than
  every instance running it. A claim that fails after its statement committed on the server
  (the 10 second claim timeout fired, or the reply was lost) leaves the job busy, with no run,
  until its lease expires. A release that fails is logged too, and the claim then blocks the job
  until its lease expires in the same way.
- **A clock far off is refused, not corrected.** A boundary more than a minute ahead of the
  database clock is refused with an error, logged at error level, so that a wrong clock cannot
  store a future boundary and block the job on every instance. Either clock can be the wrong one:
  an instance whose clock is ahead runs no recurring job until it is fixed, and a database whose
  clock is more than a minute behind refuses every instance.
- **It relies on the database keeping its commits.** A failover that loses a committed claim or
  release can run a boundary twice or let two runs overlap.

A boundary on which the id is already busy in the process is skipped there without claiming.
Otherwise the run is admitted, which makes the id busy, and the claim is made before the job body
runs. While the claim is in flight the id is busy as for a running job: `RunOnce` returns false,
`RunNext` and `Supersede` queue behind it, and `Active` counts the id. `Supersede` and shutdown
cancel the claim's context, and a claim that fails because of that is logged at debug level, not
as a failure. A store can still answer that the claim is won. The PostgreSQL one always answers a
claim it has started, since the statement would commit anyway: it runs to the end, bounded by
the 10 second claim timeout. The job is then not run and the claim is released.

A refused claim is logged at debug level, like a skipped busy boundary. It means the boundary was
already claimed by an instance, this one included after its clock stepped back or a restart on a
host whose clock is behind, or the job is still busy. On the instances that lose, the recurring
id is still admitted at each due boundary and dropped when the claim is refused, so `Active`
rises briefly there, and when that id was the only busy one, `Idled` fires.

### The table

One row per job id, so it does not grow with time and needs no cleanup:

| Column | Type | Meaning |
| --- | --- | --- |
| `job_id` | `text`, primary key | The id given to `Add`. |
| `tick` | `timestamptz` | The latest boundary claimed. |
| `busy_until` | `timestamptz`, null | When the claim stops blocking later boundaries; null once released. |

A claim is one statement: an upsert that takes the row only when its `tick` is older than the
claimed boundary and its `busy_until` is null or past, and that writes nothing when the boundary
is more than a minute ahead of the database clock. `now()` and `busy_until` come from the
database, so instances whose clocks disagree agree on whether a claim is busy. A release clears
`busy_until` only while the row still holds the boundary that instance claimed.

## Running keyed jobs on several instances

A job id excludes other runs of the id in one process: `RunOnce` on a busy id returns false,
`Supersede` cancels the run, `RunNext` waits for it. On several instances each instance only knows
its own runs. To make an id exclusive across the instances, give every instance's scheduler the
same leaser, on the same table, together with the claimer:

```go
// At startup, on every instance
err := claims.CreateTable(ctx, db.DB, "scheduler_claims")
if err != nil {
	return ez.Wrap(err)
}
err = claims.CreateLeaseTable(ctx, db.DB, "scheduler_leases")
if err != nil {
	return ez.Wrap(err)
}

s, err := scheduler.New(
	scheduler.WithClaimer(claims.NewBunStore(db.DB, "scheduler_claims")),
	scheduler.WithLeaser(claims.NewBunLeaser(db.DB, "scheduler_leases")),
	scheduler.WithLogger(log),
)
```

Set both. The leaser stops two runs of an id from overlapping, but without a claimer a recurring
job can still run once per instance per boundary: each instance asks for the lease at the
boundary, and an instance that asks after the winner's run returned wins it again.
`scheduler.Leaser` is the interface, for an app that keeps leases somewhere else. Like
`CreateTable`, `CreateLeaseTable` takes an advisory lock around the create and needs `CREATE` on
the schema; an app that cannot grant it creates the table in a migration (`job_id text PRIMARY
KEY`, `owner text NOT NULL`, `held_until timestamptz NOT NULL`, `superseded boolean NOT NULL
DEFAULT false`).

### How it works

Every run, recurring or on demand, holds the lease of its id while it runs. The run asks for it
after its recurring claim, if any, and before its `WithLimit` permit, so the order is claim, lease,
permit, job. The lease lasts 30 seconds from the database's clock. While a run holds it, its
instance renews all the leases it holds in one statement every 2 seconds. The lease is released
when the run returns, panic included.

- **`RunOnce` and recurring runs ask once.** When another instance holds the id, or the store
  fails, the job is skipped, logged at debug level for a refusal and at error level for a failure
  unless the run was cancelled. A recurring run skipped this way releases its claim.
- **`RunNext` and `Supersede` wait.** They ask every second until they win, their context ends,
  the scheduler stops, or their job timeout plus 30 seconds passes; giving up on that bound is
  logged at error level. A store failure while waiting is logged at warn level the first time and
  at debug level after that.
- **`Supersede` marks the lease until its job gets it or gives up.** Every attempt of a
  `Supersede` that is refused marks the lease superseded, so until its job wins the lease or gives
  up (its context ends, the scheduler stops, or its bound passes), every run that holds the id is
  cancelled, including runs that started after the call: the holder's own pending job, or the job
  of another `Supersede`. Of two `Supersede` calls on different instances at the same
  time, one job runs to completion and the other is cancelled, runs after it or gives up at its
  bound, in no guaranteed order.
- **A marked lease cancels its run.** The instance holding a lease marked superseded cancels the
  run that holds it with `scheduler.ErrSuperseded` when its next renewal sees the mark; the
  instance attempts a renewal every 2 seconds, and only one that succeeds sees it. A renewal
  that finds the lease no longer the run's cancels the run with `scheduler.ErrLeaseLost`, and so
  does an instance that could not renew for 20 seconds, even while a renewal is still hanging:
  that leaves the job 10 seconds to return before the database lets another instance take the id.
- **A pending job runs on the lease of the run before it.** When a run returns and a job queued
  on the same instance with `RunNext` or `Supersede` is waiting, the job runs next without
  releasing the lease, unless the lease was marked superseded or lost. Then the lease is released
  first and the job asks for one of its own, as `RunNext` or `Supersede` would.
- **A job waiting for the lease keeps its id busy on its instance.** `Active` counts it, `Idled`
  does not fire, `RunOnce` returns false, and `RunNext` and `Supersede` replace its pending job. A
  local `Supersede` cancels the waiting job, which never runs. When an attempt in flight at that
  moment still wins the lease, the replacement runs on it without asking; otherwise the
  replacement asks for the lease and marks it. Shutdown cancels running and waiting jobs as
  before, and the leases held are released as their jobs return.

### What it does not

- **The return values mean "accepted on this instance".** `RunOnce`, `RunNext` and `Supersede`
  return what they return without a leaser: a `RunOnce` that returns true can still be skipped
  because another instance holds the id, and a `RunNext` or `Supersede` that returns true can
  give up waiting.
- **`Supersede` across instances cancels whichever run holds the id when the mark is seen.**
  That is at the holding instance's next successful renewal after an attempt, which it attempts
  every 2 seconds, so later while its renewals are slow or fail, and the run
  holding the id then can be a later one than the run that was going at the call, such as a
  pending job that already took the newer work. A job cancelled with `ErrSuperseded` or
  `ErrLeaseLost` must leave the work it took for the next run.
- **A `Supersede` that gave up can still cancel a run.** A mark left by a `Supersede` whose job
  gave up, because its context ended, the scheduler stopped or its bound passed, stays on the
  lease while the holder keeps it, and is acted on at the holder's next successful renewal: it
  cancels the run then holding the lease, which can be one queued after the `Supersede` gave up,
  and no job replaces it. One process has the same edge:
  a pending `Supersede` job whose context is done by the time it would run is dropped, after its
  call already cancelled the run.
- **A `Supersede` can still wait behind a busy instance.** The holder releases the lease when its
  cancelled run returns, and its next pending job asks at once, while the `Supersede` asks once a
  second. That pending job is cancelled in turn, but an instance that keeps queueing `RunNext`
  for the id can win each release, and the `Supersede` then gives up at its bound.
- **A job must return within 10 seconds of being cancelled.** A run cancelled because its
  instance could not renew the lease has that long before the database can let another instance
  take the id while the job still runs.
- **A frozen process can overlap its successor.** A process stopped by a pause of its VM or
  container cannot renew or cancel meanwhile. A job cancelled on waking from a freeze has only
  what is left of the 30 seconds from its last good renewal to return: less than 10 seconds when
  the freeze ended more than 20 seconds after that renewal, and nothing when it ended more than 30
  seconds after it, when another instance may already run the id beside it. Work that must never
  overlap needs its own guard in the database, such as a row lock or a unique constraint.
- **Pending jobs live in the process that queued them.** Two instances can each hold a pending
  job for one id; each runs when it wins the lease, unless it gives up first. A crash loses that
  instance's.
- **`WithLimit` stays per instance.** A job waiting for a permit holds its lease meanwhile, so
  another instance with free permits cannot run the id.
- **Ids are app-wide.** A keyed job meant to run on every instance, such as a local cache refresh,
  needs a scheduler without a leaser, or an id per instance.
- **Two schedulers in one process behave as two instances.** Given the same lease table, they
  exclude each other by id like two processes.
- **Instances without the leaser do not take part.** During the rolling deploy that adds
  `WithLeaser`, instances on the earlier build run their ids without asking for leases.
- **A store that does not answer skips or delays runs.** An acquire that failed is released, best
  effort, when the job gives up on the lease, since it may have committed. The PostgreSQL leaser
  answers an acquire it has started even when the run is cancelled on the way, by a `Supersede`
  or a shutdown, but one that runs out of its 10 second timeout can commit after the release was
  made, and then no run holds the id until the lease lapses after its 30 seconds. A failed release
  is logged at error level and the lease lapses the same way; so does the lease of an instance
  killed mid-run.
- **Renewals use the connection pool the leaser was given.** A renewal waits at most 2 seconds
  for a connection. On a pool with a connection limit that jobs keep exhausted for 20 seconds, no
  renewal gets through and every leased run on the instance is cancelled with `ErrLeaseLost`.

### The lease table

A row per job id while its lease is held. The release deletes it; the row of a lease that lapsed
without a release stays until another owner takes it or a release sweeps it:

| Column | Type | Meaning |
| --- | --- | --- |
| `job_id` | `text`, primary key | The job id. |
| `owner` | `text` | The scheduler's random token and a counter, unique per acquisition. |
| `held_until` | `timestamptz` | When the lease lapses unless renewed. |
| `superseded` | `boolean` | Set by a refused `Supersede`; cleared when another owner takes the row. |

Each operation is one statement on the database's `now()`, with no session state, so it works
through PgBouncer in transaction mode. An acquire is an upsert that takes the row when it is the
same owner's or its `held_until` passed. A renewal extends only rows that still name their owner.
A release deletes the row only while it names the releasing owner, and in the same statement
deletes the rows whose lease lapsed more than an hour ago, left by instances that stopped for ids
no longer used.

The statements are written for the default `read committed` isolation only. Under a database or
role default of `repeatable read` or `serializable`, acquires, renewals and releases can all fail
with a serialization error when acquires race the holder: a contended acquire fails instead of
being refused, a failed renewal fails that instance's whole batch, and a failed release leaves the
id blocked until the lease lapses. Two runs still never hold the id together.

The job id is the primary key, so it must fit in a btree index entry, about 2.7 KB after
PostgreSQL's compression, and must not contain a NUL byte, which PostgreSQL's `text` refuses. Every
acquire of an id that breaks either limit fails, so its jobs never run.
