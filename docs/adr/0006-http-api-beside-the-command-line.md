# An HTTP API beside the command line, for callers with no shell

plugyard gains an HTTP API, served by `plugyard api up`. It offers the actions of `plugyard exec` and reading chargers, and nothing else. The V1 specification listed "an HTTP API" among the things left out, so this reverses that line.

We chose this because of how plugyard came to be used: on a staging server, for weeks, with a handful of chargers that the developers of apps and dashboards test against. Those developers have no shell on the server. With the command line as the only way in, nobody but the person with a shell could plug an EV in again after a session, or cause a fault to look at.

## Considered options

- **A standing state in the site file**: each charger entry says what it should be, such as "EV plugged in, plugged in again two minutes after every session", and plugyard keeps it so. It needs no network surface, but it only helps with states someone foresaw, and gives nobody a way to act. An API is the more general piece: a standing state can be built on it with a short script on a timer, and not the other way round.
- **`repeat` on a scenario.** Less new surface, but the state of the chargers then lives in a timeline, and drifts for the rest of the run when one step fails or the CSMS restarts.
- **The API inside each site's process**, as `site up --api`. One process per site suits a container, but the command line is about chargers across every site, two sites would mean two addresses, and every charger started with `plugyard run` is a site of its own.
- **An MCP server alone**, for AI agents. People and scripts need the same reach, and an MCP server can be laid over an HTTP API later.

## Consequences

- **The API is a process of its own**, started and stopped with `plugyard api up` and `plugyard api down`. It serves every charger on the machine, as the commands do, and reaches the sites over the same sockets (ADR 0004). Sites know nothing of it, and it keeps running while they come and go. There is one per user and machine. This is one more long-running process, but still no daemon in the sense of ADR 0001: nothing depends on it, and the commands work without it.
- **What it offers is narrow, and stays so.** It acts on chargers and reads them. It never takes a file path, starts or removes a site or a charger, changes where a charger connects to, updates or removes plugyard, or manages its own tokens. Those are the ways from "can call the API" to "owns the machine", and they stay on the command line.
- **A call does what the command does.** The API hands a call to the code behind `exec`, `ps` and `inspect`, so the two cannot drift apart.
- **It listens on this machine only, unless told otherwise.** Listening on another address is refused while no token exists.
- **Tokens have names**, are shown once and kept as hashes, can be limited to one site, and can be read-only. The name is what the log of calls shows.
- **Every call is recorded**, also the ones turned away: when, which token, from where, what, on which charger, how it ended.
- **A web page in a browser cannot act through it.** An API on localhost with no token would otherwise let any website a developer visits unplug a charger. A call must name this machine as its host, must not come from a page of another origin, and an action must say that its body is JSON.
- **Encryption is optional**, with `--tls-cert` and `--tls-key`, since the usual place for it is a reverse proxy in front.
- **The address scheme is versioned**, `/v1/...`, and described by an OpenAPI document that `plugyard api spec` prints and the tests hold to the server.
- An MCP server and a Docker image are to follow, on top of this. (The MCP tools came with ADR 0007.)
