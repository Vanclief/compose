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
  wait for a `WithLimit` permit and the time the job takes to return once its job timeout cancels
  it add up to more than that
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
  can overlap: "a due boundary is skipped while the id is busy" holds per instance.
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
