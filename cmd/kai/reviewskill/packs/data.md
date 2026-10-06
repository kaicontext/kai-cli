## Schema, migrations and queries (this change touches stored data)

- **Rollback:** a migration that cannot be rolled back, or that destroys data on the way down.
- **Locks:** a migration that locks a large table (an index built without concurrency, a column rewrite, a default on a big table).
- **New columns:** a non-null column added without a default or backfill, and a backfill that is not batched.
- **Code and schema out of step:** code that reads a column before the migration that adds it has run, or writes one after the migration that drops it.
- **Query conditions:** a query missing a tenant, owner or soft-delete condition its siblings apply, a filter on a column with no index, and a unique assumption with no unique constraint.
- **Encoding:** data written in one encoding or normalization and read in another (case, trimming, time zone, id kind).
