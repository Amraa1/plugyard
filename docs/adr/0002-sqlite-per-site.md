# Charger state in one SQLite database per site, through a pure-Go driver

Charger state is stored in one SQLite database file per site in the per-user data directory, accessed through a pure-Go driver (`modernc.org/sqlite`). SQLite gives atomic writes, so a machine shutdown mid-write cannot corrupt state, and it lets a command such as `plugyard ps -a` or `plugyard inspect` read the file while the running site writes to it, which matters because there is no daemon acting as the single owner (ADR 0001).

## Considered options

- **JSON files plus an embedded key-value store (bbolt)**, which is how Docker stores container and volume state. Rejected because bbolt allows only one process to hold the file open for writing with no concurrent readers from other processes, and because thousands of per-charger JSON files rewritten whole on every change scale poorly.
- **The C-based SQLite driver (`mattn/go-sqlite3`)**. Rejected because it needs cgo and a C toolchain per target, which breaks building all three platforms from one machine. The pure-Go driver is slower, which is irrelevant at this data volume.

## Consequences

- OCPP message logs are kept out of the database, in rotating plain files beside it.
- Schema changes between releases need migrations.
