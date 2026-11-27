# plugyard V1 specification

Status: agreed and reviewed design, partly implemented. Progress is tracked in the milestone issues.
Terms in **bold** on first use are defined in [CONTEXT.md](../CONTEXT.md).

## 1. Purpose

`plugyard` is a terminal tool that simulates EV chargers so that a **CSMS** can be developed and tested without hardware. Its command line follows Docker's conventions: a file describes a kind of charger, a YAML file starts many of them together, and commands such as `ps`, `logs` and `exec` operate on the running ones.

- Repository and Go module: `github.com/Amraa1/plugyard`
- License: Apache-2.0
- Language: Go
- Platforms: Windows, Linux, macOS (x86-64 and ARM64)

### Users

1. **Primary: CSMS developers** running one charger or many against a local or staging CSMS.
2. **Secondary: QA and CI**, driving the tool from scripts.
3. **AI agents**, such as coding assistants, driving the tool for one of the above (section 12).

### Scale

One site must run hundreds to a few thousand chargers comfortably on a developer laptop.

## 2. Scope

### In V1

- OCPP 1.6J over WebSocket
- Feature profiles: Core (complete), Firmware Management, Remote Trigger, Smart Charging
- `ws://`, and `wss://` with HTTP Basic Auth (security profile 2), with an option to skip certificate verification
- A minimal EV model that produces realistic meter values
- Chargers defined in files, driven by commands and by scenarios
- Charger state saved to disk, surviving stops, terminal exits and machine shutdowns
- An HTTP API for callers with no shell on the machine, added after V1 (section 14, ADR 0006)
- Tools for AI agents over the Model Context Protocol (MCP), served by the API, added after V1 (section 14, ADR 0007)
- Declared state in a site file, for a site that is left running for others to look at, added after V1 (section 6, ADR 0009)

### Not in V1

- OCPP 2.0.1 (the protocol sits behind a seam so it can be added)
- Local Auth List and Reservation profiles; the charger answers `NotSupported`
- Fetching firmware: a firmware update reports its statuses, and no file is downloaded
- Logging in to an SFTP server with a key, and checking an SFTP server's key against one known beforehand
- Client certificates (security profile 3)
- ISO 15118 and Plug & Charge
- EVs as persistent entities
- Load-testing features: ramp-up profiles, throughput reports, multi-machine runs
- Deliberate protocol violations (malformed messages, missing or late responses, wrong ordering)
- A build step, a model store or a registry
- A background daemon or system service; automatic start after reboot
- Through the HTTP API: anything but actions on chargers and reading them (section 14)
- An MCP server that the agent's client starts on its own machine; the command line and the guide serve an agent there (section 12)
- Assertions, conditions or loops in scenarios
- Commands `events`, `pause`, `attach`
- Waiting for an OCPP message or a log line; `wait` waits for a state

## 3. Concepts

| Concept | Meaning |
|---|---|
| **Chargerfile** | A YAML file declaring one model |
| **Model** | A kind of charger: vendor, model name, firmware, connectors, power, default configuration, behavior settings |
| **Charger** | One simulated instance, known to the CSMS only by its **charge point ID** |
| **Name** | A second handle for a charger, used only on the command line and unique on the machine. It is given by the user or made up, as Docker names containers |
| **Connector** | A numbered outlet on a charger (1, 2, ...), as in OCPP 1.6 |
| **Site** | A named group of chargers declared in a site file and started together |
| **EV** | The simulated vehicle on a connector; four parameters that exist only while plugged in |
| **EV preset** | A named set of EV parameters in a site file |
| **Charge curve** | The share of its maximum power an EV accepts at each state of charge |
| **Scenario** | A file of timed actions run against a running site |
| **Declared state** | The state a site file says a charger is in each time it powers on |

Every charger belongs to a site. A charger started with `plugyard run` belongs to an implicit site named after its charge point ID.

## 4. Command reference

Rules that apply to every command:

- Required values are positional arguments; optional values are flags.
- A charger argument is a charge point ID or a name. It also accepts a glob pattern (`'FAST-*'`, `'*'`), which is matched against both and acts on every match.
- Charger commands see chargers from all sites. `-s, --site <name>` narrows to one site.
- The same charge point ID may exist in two sites. A command whose charger argument is ambiguous fails and asks for `-s` or the charger's name. A name is never ambiguous.
- Output is labelled with the charge point ID, since that is what the CSMS shows. The name is shown by `ps`.
- `--format json` is available on `ps`, `site ls`, `inspect`, `stats`, `logs`, `site logs`, `info`, `scenario run`, `exec`, `start`, `stop`, `restart`, `kill` and `rm`. It prints one JSON object per line, as Docker does. `inspect` and `info` print a single object. Fields are only ever added to these objects, never renamed or removed.
- Output is colored when it goes to a terminal: green for `ok`, `online`, `running` and `Charging`; yellow for `connecting`, `Preparing`, `Finishing` and the suspended statuses; red for `failed`, `offline` and `Faulted`. Color is never the only signal, and it is switched off when output is piped or redirected or when the `NO_COLOR` environment variable is set.
- Exit codes: `0` success, `1` runtime failure, `2` usage or validation error.
- Starting a site whose name is already running fails with exit code `1`. This includes a second `plugyard run` with the same charge point ID.

### Site commands

Site commands act on the `site.yaml` in the current directory unless `-f, --file <path>` or `-s, --site <name>` says otherwise.

| Command | Effect |
|---|---|
| `plugyard site up [-d] [-y] [--speed N] [--connect-rate N] [--api <address>]` | Read the site file, create the site's chargers, reusing saved state, and start all of them but those declared `stopped` (section 6). Runs in the foreground showing the log of every charger, as `docker compose up` does; `-d` runs it in the background. `--api` also serves the API while the site runs (section 13). |
| `plugyard site stop` | Power off all chargers. They stay listed in `ps -a`. State is kept. |
| `plugyard site start` | Power on every charger of a stopped site, in the background, as saved, or in its declared state where it has one. Those declared `stopped` stay off. No file is read. |
| `plugyard site down [--reset]` | Stop and remove the chargers from the listing. Saved state is kept unless `--reset` is given, which wipes it. |
| `plugyard site ls [-a]` | List running sites; `-a` includes sites that are `stopped` or `down`. |
| `plugyard site logs [-f] [--events] [--since <when>] [--tail N]` | Show the log of every charger of the site. Here `-f` follows, as in `docker compose logs`; another site file is named with `--file`. |
| `plugyard site validate` | Check the site file and the Chargerfiles it references without starting anything, and list the chargers it would create with their charge point IDs. |

- `site up -d` starts a background copy of plugyard, waits until it reports the site running, and returns (see ADR 0005). Its chargers log to the site's log files like those of any other site; `site logs -f` follows them. It keeps running after the terminal closes, and ends when the user signs out.
- `site stop` and `site down` shut a site down in an orderly way: transactions end with `StopTransaction`, then connections close. They work on a site in the foreground of another terminal as well as one in the background. Pressing Ctrl+C in a foreground `site up` does the same as `site stop`.
- `site up` and `site start` differ as in Docker compose. `site up` reads the site file again and applies what changed. `site start` reads nothing and works from any directory with `-s <name>`. A site that was taken down is refused by `site start`, with a message to use `site up`.
- A site is `running` while its process lives, `stopped` when it has saved state and no process, and `down` after `site down`. A site whose process was killed or crashed shows as `stopped`.
- `site down --reset` does not ask for confirmation.
- Every charger connects at once by default. `--connect-rate N`, or `connectRate:` in the site file, limits new connections to N per second; `0` means no limit.
- On Linux and macOS each connection uses one of the process's open files. `site up` raises that limit itself where the system allows, and otherwise stops with a message giving the `ulimit -n` value needed.
- In the foreground log, each charge point ID has its own color on a terminal.

### Charger commands

| Command | Effect |
|---|---|
| `plugyard ps [-a]` | List running chargers with name, site, connection state (`connecting`, `online` or `offline`) and the status of each connector; `-a` includes stopped ones, shown as `stopped` with the status each connector will report on start, by its saved and its declared state. |
| `plugyard wait <charger>... <state> [-n, --connector <number>] [--timeout <time>]` | Wait until every named charger is in a state (see below). |
| `plugyard logs <charger> [-f] [--events] [--since <when>] [--tail N]` | Show events and OCPP messages (section 11). |
| `plugyard stats [--no-stream]` | Live table of power, energy on the meters, transactions and message counts per running charger, with a total first. |
| `plugyard inspect <charger>` | Full state of one charger: model, connectors, EVs, transactions, charging profiles, configuration keys and the messages still owed to the CSMS. Works on a stopped charger. Never shows a password. |
| `plugyard start <charger>` | Power on. |
| `plugyard stop <charger>` | Orderly shutdown. State is kept. |
| `plugyard restart <charger>` | `stop` then `start`. |
| `plugyard kill <charger>` | Power cut. State is kept as it was last saved. |
| `plugyard rm [-f] <charger>` | Wipe the charger's saved state. Refuses a running charger unless `-f`, which stops it first. The charger is recreated fresh on the next `site up` if the site file still declares it. |
| `plugyard run <Chargerfile> --csms <url> --id <charge point ID> [-d]` | Start one charger without a site file, in an implicit site named after the ID. |

**The `charger` suite.** Every charger command also exists under `plugyard charger`, as Docker has `docker container ls` beside `docker ps`. A suite command is the short command under a second name, with the same flags, output and exit codes:

| Suite command | Short command |
|---|---|
| `plugyard charger ls` | `plugyard ps` |
| `plugyard charger exec`, `inspect`, `logs`, `stats`, `wait`, `run` | `plugyard exec`, `inspect`, `logs`, `stats`, `wait`, `run` |
| `plugyard charger start`, `stop`, `restart`, `kill`, `rm` | `plugyard start`, `stop`, `restart`, `kill`, `rm` |

The command line thus has three suites: `site`, `charger` and `api` (section 14). The short commands are the ones this document uses.

`start`, `stop`, `restart`, `kill` and `rm` follow Docker, and print one line per charger as `exec` does:

- `stop` asks, `kill` cuts, and only `rm` deletes. A killed charger still exists and comes back with `start`.
- Stopping, killing or removing the last running charger of a site ends the site's process; the site is then `stopped`.
- `start` on a charger whose site has no process starts the site in the background with only the named chargers powered on. The others stay stopped until they are started or `site start` is run.
- `stop` on a charger that is already stopped succeeds. `kill` on one fails.
- `rm` does not ask for confirmation. Removing the last charger of a site removes the site.

`run` stays in the foreground printing the charger's log until interrupted, then shuts the charger down in an orderly way. With `-d, --detach` it starts the charger in the background as `site up -d` does, prints how to follow its log and how to stop it, and returns; `plugyard stop <ID>` shuts it down. `--detach` and `--rm` cannot be used together, since a charger in the background runs from its saved state. Its state is saved like that of any other charger: running the same charge point ID again brings the same charger back, and `plugyard rm <ID>` wipes it. `--name <name>` gives the charger its name. `--rm` saves nothing, for a throwaway charger. `-y` answers the question `site up` asks when the Chargerfile no longer fits the saved state. It also accepts `--speed N`, with the same meaning as on `site up`, `--password <password>` for HTTP Basic Auth, which falls back to the `PLUGYARD_CSMS_PASSWORD` environment variable so the password can stay off the command line, `--insecure-skip-verify`, and `--allow-upload <host>`, which may be repeated, and `--upload-insecure-skip-verify`, which mean what `uploads.allow` and `uploads.insecureSkipVerify` mean in a site file.

### Actions

```
plugyard exec <charger> <action> [arguments] [-n, --connector <number>]
```

| Action | Arguments | Effect |
|---|---|---|
| `plug` | `--ev <preset>`, `--battery`, `--soc`, `--max-power`, `--target-soc`, `--curve <name>` | Plug an EV into the connector |
| `unplug` | | Remove the EV |
| `tap` | `<idTag>` | Present an RFID tag: `Authorize`, then start or stop the transaction |
| `fault` | `<errorCode>`, `--info <text>`, `--vendor-code <code>` | Report a fault with `StatusNotification` |
| `clear` | | Recover from the fault |
| `offline` | `--for <duration>` | Drop the network connection; the charger keeps running |
| `online` | | Restore the network connection |
| `reboot` | | Reboot the charger |
| `fail` | `<step>`: `download`, `install` or `upload` | Make the next firmware download, firmware installation or diagnostics upload fail |

- `--connector` defaults to 1 for `plug`, `unplug`, `tap`, `fault` and `clear`.
- `fault` and `clear` also accept `--connector 0`, which means the charger as a whole, as in OCPP.
- Giving `--connector` to a whole-charger action (`offline`, `online`, `reboot`, `fail`) is an error.
- `plugyard exec --help` lists the actions and the error codes.
- `exec` waits for the immediate outcome of the action and prints it, for example that a tag was accepted and a transaction started, or that the CSMS rejected the tag.
- The exit code is `0` when the action was delivered, whatever the CSMS answered. A rejected tag is the CSMS's answer and not a failure. Exit code `1` means the action could not be carried out, for example because no charger matched or the connector already has an EV.
- `exec` prints one line per matched charger, sorted by charge point ID: the ID, the connector, `ok` or `failed`, and the outcome or the reason. When the charger argument matched several chargers the action runs on every one of them, a summary line follows (`18 of 20 succeeded, 2 failed`), and the exit code is `1` if any failed. Chargers on which the action worked are not rolled back.
- `exec` also accepts `--format json`.

### Waiting for a state

```
plugyard wait <charger>... <state> [-n, --connector <number>] [--timeout <time>] [-s, --site <name>]
```

`wait` returns when every named charger is in the state. A script uses it in place of a guessed sleep between an action and a check.

- The state is one of:

  | Kind | States | About |
  |---|---|---|
  | Connector status | `Available`, `Preparing`, `Charging`, `SuspendedEV`, `SuspendedEVSE`, `Finishing`, `Faulted`, `Unavailable` | Connector 1, or the one `--connector` names |
  | Connection state | `online`, `offline`, `connecting`, `stopped` | The whole charger, as `ps -a` shows it; `--connector` is an error |

  Upper and lower case are not told apart.
- Several chargers may be given, each by its charge point ID, its name or a pattern. `wait` returns when all of them are in the state at the same moment.
- `--timeout` is how long to wait, `30s` unless given; `0` waits without limit. It is real time, whatever the `--speed` of the site.
- Exit code `0`: the state was reached, or the chargers were in it already. One line per charger is printed, such as `CP-1: connector 1 is Charging`.
- Exit code `1`: the time ran out. One line for each charger that is not in the state is printed to standard error, such as `CP-1: connector 1 is Preparing, not Charging, after 30s`.
- Exit code `2`: the state is not one of the list, an argument matches no charger, or a charger has no connector of that number.
- A stopped charger is `stopped` and has no connector status, so waiting for one on it runs out of time.

### Other commands

| Command | Effect |
|---|---|
| `plugyard scenario run <file> [-s <site>] [--fail-fast] [--step N] [--from N] [--to N] [--speed N]` | Run a scenario against a running site (section 10). |
| `plugyard scenario validate <file> [-s <site>]` | Check a scenario and show its plan without running it. |
| `plugyard init [directory] [--csms <url>] [--force] [--agents]` | Write a starter `site.yaml`, `Chargerfile` and `scenario.yaml`; with `--agents`, also install the skill for AI agents (section 12). |
| `plugyard agents` | Print the guide for AI agents (section 12). |
| `plugyard schema <chargerfile\|site\|scenario>` | Print the JSON schema of a file type. |
| `plugyard version` | Print the version and nothing else, such as `v0.1.1`, so that a script can use it as it is. |
| `plugyard info` | Print the version, the OCPP versions plugyard speaks, the Go version and system it was built for, the data directory, and every site with its status, charger counts and CSMS URLs. This is the output to include in a problem report. |
| `plugyard update [--check] [-y, --yes]` | Replace the program with the latest release, restarting what is running (section 13). |
| `plugyard uninstall [-y, --yes] [--keep-data]` | Remove the program, and the saved chargers and logs (section 13). |

`stats` behaves as `docker stats` does:

- In a terminal it refreshes every second until Ctrl+C, busiest chargers first, showing as many as fit on the screen.
- With `--no-stream`, with `--format json`, or when the output is not a terminal, it prints once, with every charger, sorted by charge point ID.
- Power is what the charger delivers now over all its connectors, and energy is the sum of its meter registers.
- Message counts are since the charger was powered on. They are not saved.

## 5. Chargerfile

YAML, in a file named `Chargerfile` or `*.chargerfile.yaml`. A JSON schema is published for editor support (section 12).

```yaml
vendor: Acme
model: FastOne 50
firmware: 1.4.2
ocpp: 1.6                    # the OCPP version the charger speaks
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
  - { id: 2, type: CHAdeMO, maxPower: 50kW }
config:                      # OCPP configuration keys and their defaults
  HeartbeatInterval: 300
  MeterValueSampleInterval: 60
dataTransfer: [com.acme]     # vendor IDs whose DataTransfer requests are accepted
voltage: 230                 # volts; with phases, turns a limit in amps into power
phases: 3                    # 1, 2 or 3
onEvFull: stop               # stop | suspend
startOn: tap                 # tap | plug
startTag: FREEVEND           # idTag used when startOn is plug
```

| Field | Meaning |
|---|---|
| `vendor`, `model`, `firmware` | Sent in `BootNotification` |
| `ocpp` | The OCPP version the charger speaks. `1.6`, also written `1.6J`, is the default and the only value for now. The version belongs to the model, so that one site can later mix chargers of different versions |
| `connectors` | Numbered outlets with type and maximum power |
| `config` | Default values of OCPP configuration keys. A key that is not one of the charger's own is kept as text, reported by `GetConfiguration` and changeable by the CSMS |
| `dataTransfer` | Vendor IDs for which a `DataTransfer` request from the CSMS is answered `Accepted`. Any other vendor ID is answered `UnknownVendorId` |
| `voltage`, `phases` | The electrical supply, used only to turn amps into power and back: power = amps x `voltage` x `phases`. Defaults: 230 and 3. A single-phase wallbox sets `phases: 1`; a DC charger can set its DC voltage and `phases: 1` |
| `onEvFull` | `stop` (default): end the transaction and go to `Finishing`. `suspend`: report `SuspendedEV` and keep the transaction open. |
| `startOn` | `tap` (default): a transaction starts when a tag is presented. `plug`: it starts on plug-in using `startTag`. |

## 6. Site file

Default filename `site.yaml`.

```yaml
name: depot-north
csms:
  url: wss://csms.example.com/ocpp     # the charge point ID is appended
  password: ${CSMS_PASSWORD}
  insecureSkipVerify: false
speed: 1
connectRate: 0                         # new connections per second; 0 = no limit
log: { maxSize: 10MB, files: 3 }       # the cap of the site's log; these are the defaults
uploads: { allow: [files.example.com] } # where else diagnostics may be uploaded to (section 8)
evs:
  compact:   { battery: 40kWh, maxPower: 50kW }
  longrange: { battery: 82kWh, maxPower: 230kW, soc: 10%, targetSoc: 80%, curve: dc-fast }
chargers:
  fast:
    model: ./fast.Chargerfile
    count: 20
    id: "FAST-{n:03}"                  # FAST-001 ... FAST-020
    ev: longrange                      # default EV for these chargers
  lobby:
    model: ./ac.Chargerfile
    id: "LOBBY-1"
    name: front-desk                   # optional; a name is made up otherwise
    config: { MeterValueSampleInterval: 10 }
```

- `name` identifies the site and must be unique per user.
- `csms` applies to the whole site; a charger entry may override any part of it.
- Each entry under `chargers` instantiates a model `count` times (default 1), with optional `config` overrides and a default `ev`.
- Entry names (`fast`, `lobby`) are used only inside the file. Everywhere else a charger is known by its charge point ID or its name.
- `name` gives a charger its name, and is allowed only on an entry that creates one charger. Every other charger gets a made-up name of the form `adjective_noun`, such as `brisk_volt`, when it is first created. A name is letters, digits, `_`, `.` and `-`. It stays with the charger until `rm` or `--reset`, and a name already in use on the machine is refused.
- A Chargerfile path is relative to the site file.
- `uploads.allow` lists hosts, by name or address, that diagnostics may be uploaded to besides the machine the CSMS is on (section 8). An entry is a host alone: no scheme, port or path. `uploads.insecureSkipVerify: true` turns off the check of the certificate of an `https` or `ftps` server.
- `log` caps the site's log (section 11): at most `files` files of `maxSize` each. `maxSize` takes `kB`, `MB` or `GB` and is at least `1kB`; `files` is from 1 to 100.
- Unknown settings are errors, and `site validate` reports every problem at once.

### Charge point IDs

An entry gets its charge point IDs from an **ID template** or from a list.

| Setting | Meaning |
|---|---|
| `id: "FAST-{n}"` | `{n}` is the charger's number, counting from 1: `FAST-1`, `FAST-2`, ... |
| `id: "FAST-{n:03}"` | `{n:03}` pads the number with zeros to three digits: `FAST-001` |
| `start: 202310090165` | Count from this number instead of 1, for IDs that look like serial numbers |
| `ids: [12994897, p025b2036387]` | An explicit list, for IDs that follow no pattern. Replaces `id`, `count` and `start` |

- An entry with `count` above 1 must have `{n}` in its template.
- The same charge point ID twice in one site file is an error.

### Declared state

An entry may declare the state its chargers are in each time they power on. It is for a site that is left running for others to look at, such as one on a shared staging server that must show an example of every charger state at all times.

```yaml
chargers:
  lobby:
    model: ./wallbox.Chargerfile
    id: "LOBBY-1"
    state:
      connectors:
        1: { ev: longrange }           # an EV is plugged in: Preparing
  wall:
    model: ./wallbox.Chargerfile
    id: "WALL-1"
    state:
      connectors:
        1: { ev: longrange, renew: 5m }   # and a fresh one comes when it is full
  bay:
    model: ./Chargerfile
    id: "BAY-1"
    state:
      connectors:
        0: { fault: InternalError }    # the charger as a whole
        2: { fault: { code: GroundFailure, info: RCD tripped, vendorCode: E42 } }
  free:
    model: ./Chargerfile
    id: "FREE-1"
    state:
      connectors:
        1: {}                          # nothing plugged in and no fault: Available
  gone:
    model: ./Chargerfile
    id: "GONE-1"
    state: { connection: offline }     # stays away across restarts
  spare:
    model: ./Chargerfile
    id: "SPARE-1"
    state: { connection: stopped }     # declared and never started
```

| Setting | Meaning |
|---|---|
| `connection: offline` | The charger powers on with its network connection taken down, as after `exec <charger> offline`, and stays away until `exec <charger> online` |
| `connection: stopped` | `site up` and `site start` create the charger and leave it powered off. `plugyard start <charger>` powers it on until it is stopped |
| `connectors` | Connectors by number. `0` is the charger as a whole, as with `exec --connector 0` |
| `ev: <preset>` | An EV of that EV preset is plugged in. Not for connector `0` |
| `renew: <time>` | The declared EV is renewed when it is done (below). A time such as `30s`, `5m` or `1h30m`. Only beside `ev` |
| `fault: <errorCode>` | The connector reports the fault. The long form gives `code`, `info` and `vendorCode`, as the `fault` step of a scenario does |

- **Declared state is applied every time the charger powers on**, by any command: `site up`, `site start`, `start`, `restart`, and a start after `kill`. Where it speaks, it takes the place of the saved state (ADR 0009). A reboot and a `Reset` from the CSMS are not a power-on and apply nothing.
- **A change made by hand lasts until the charger next powers on.** `plugyard restart <charger>` puts one charger back in its declared state.
- **A connector that is listed is declared whole**: with no `ev` nothing is plugged in, and with no `fault` it has none. A connector that is not listed follows the saved state.
- **What the CSMS set is not declared and still persists.** A connector it made `Unavailable` stays so, and meter readings are untouched.
- **A declared EV is a new arrival each time**: a fresh EV at the preset's starting state of charge, in place of any EV in the saved state. Its parameters are worked out as for a plug that names the preset. On a model with `startOn: plug` a transaction starts with `startTag` at power-on, and the CSMS hears of it once the charger has booted.
- **A declared EV with `renew` is renewed when it is done**, so that its connector stays of use with nobody running `unplug` and `plug`: `1: { ev: longrange, renew: 5m }`.
  - An EV is done when it is at its target and its connector has no transaction. One whose transaction ended before it was full is not done: it can be charged again without unplugging (section 8).
  - The EV leaves after the `renew` time, as `unplug` does, and a fresh one arrives after the same time again, as `plug` does. The connector reports `Finishing`, `Available`, `Preparing`.
  - The time is real time, whatever the speed of the site, so that each status lasts long enough to be seen.
  - With `startOn: plug` the fresh EV starts a transaction, and the connector keeps cycling through `Charging`.
  - With `onEvFull: suspend` the transaction stays open, so the EV is not done and the connector stays `SuspendedEV`.
  - Only the declared EV is renewed. An EV plugged in by hand never is, and it takes the place of a fresh one that was on its way. When the declared EV is unplugged by hand, nothing arrives until the charger next powers on.
  - This is the one part of declared state that acts between power-ons.
- **A site whose chargers are all declared `stopped`** comes up with none powered on. `site up` and `site start` say so, with exit code `0`.
- On an entry with `count` above 1 the block applies to every charger of the entry.
- `inspect` shows a charger's declared state, and `site validate` shows it beside each entry. `ps -a` shows a stopped charger with the statuses its declared state gives it.

### Environment variables

| Form | Meaning |
|---|---|
| `${VAR}` | The value of the environment variable. It is an error if the variable is not set, so that a missing password or URL is caught at once and named |
| `${VAR:-fallback}` | The value of the variable, or `fallback` when it is not set or empty |
| `$$` | A literal dollar sign |

## 7. EV model

| Parameter | Flag | In a preset | Built-in default |
|---|---|---|---|
| Battery capacity | `--battery` | `battery` | 60 kWh |
| Starting state of charge | `--soc` | `soc` | 20% |
| Maximum power accepted | `--max-power` | `maxPower` | none; the connector's maximum applies |
| Target state of charge | `--target-soc` | `targetSoc` | 100% |
| Charge curve | `--curve` | `curve` | `standard` |

Precedence: flags on `plug`, then the preset named by `--ev`, then the charger entry's default `ev`, then the built-in defaults. Each level replaces only the parameters it gives.

### Charge curve

A charge curve says what share of its maximum power the EV accepts at each state of charge. It is a list of points joined by straight lines.

- The curve belongs to the EV. Delivered power is the lowest of the connector's maximum, the EV's maximum times the curve, and any active charging profile limit. An EV with no maximum of its own takes the connector's maximum as its own.
- A fast-charging EV on a slower connector therefore charges at the connector's full power for as long as its curve stays above that, as real ones do.
- Charging ends when the target is reached.
- There are no charging losses: the energy on the charger's meter equals the energy added to the battery.

Built-in curves:

| Name | Shape |
|---|---|
| `standard` | 100% up to 80% state of charge, then falling to 10% at 100%. The default |
| `dc-fast` | 30% at 0%, rising to 100% at 10%, held to 50%, falling to 28% at 80% and 7% at 100% |

An EV preset names a built-in curve or gives its own points as pairs of state of charge and power share:

```yaml
evs:
  mycar:
    maxPower: 150kW
    curve:
      - [0%, 50%]
      - [20%, 100%]
      - [60%, 100%]
      - [100%, 10%]
```

A curve must start at 0% and end at 100% state of charge, and its power share must stay above 0%, so that an EV can always reach its target. `plug --curve` accepts the names of built-in curves only.

## 8. Charger behavior

### Lifecycle

| Trigger | Simulates | What the CSMS sees |
|---|---|---|
| `start` | Power on | `BootNotification`, then `StatusNotification` for each connector |
| `stop` | Orderly shutdown | Active transactions end with `StopTransaction` (reason `Other`), then the connection closes. If the CSMS cannot be reached, the message is kept and sent after the next start |
| `kill`, a crash, or the machine losing power | Power cut | The connection drops with no message. On the next start the charger boots and sends `StopTransaction` with reason `PowerLoss`, the last saved meter reading and the time of that reading |
| `exec ... offline` | Network loss | The connection drops. Charging continues, and transaction messages are queued and replayed in order on `online` |

- A charger that starts and finds a transaction still open in its saved state was not shut down in an orderly way, whatever the cause, and reports that transaction with `PowerLoss`. No promise is made that a machine shutdown is orderly: Linux and macOS give a process a moment to finish, in which plugyard ends the transactions properly, and a background site on Windows gets none.
- The EV stays plugged in across a stop or a power cut, with its state of charge. After the start the connector reports `Finishing` until the EV is unplugged or a new transaction starts. No transaction starts by itself, even with `startOn: plug`.
- A charger with declared state powers on in it, whatever was saved (section 6). An EV it declares is a new arrival, and that one does start a transaction with `startOn: plug`.
- Messages still owed to the CSMS are saved, and are sent in order after the next boot, before anything new.

### Connection

- The charger asks for the `ocpp1.6` WebSocket subprotocol. If the CSMS does not select it, the charger logs a warning and carries on.
- If the CSMS answers `BootNotification` with `Pending` or `Rejected`, the charger sends nothing else and repeats `BootNotification` after the interval the CSMS gave.
- The heartbeat interval is the one in the `BootNotification` answer, or the `HeartbeatInterval` configuration key when the answer gives none.
- When the connection is lost or cannot be made, the charger keeps retrying, waiting 1 second at first and doubling up to 30 seconds, and boots again once connected.

### Transactions

- With `startOn: tap`, a transaction starts after plug-in and an accepted tag, in either order, and stops on a second tap of the same tag, on unplug, or on a remote stop.
- When a tag is accepted before an EV is plugged in, the connector reports `Preparing` and waits for the number of seconds in the `ConnectionTimeOut` configuration key (default 60). If no EV arrives it returns to `Available` and forgets the tag.
- A different tag presented during a transaction is sent to the CSMS with `Authorize`. It stops the transaction only if it is accepted and has the same `parentIdTag` as the tag that started it; otherwise it is logged and ignored.
- If the CSMS answers `StartTransaction` with a tag status other than `Accepted`, the charger stops the transaction at once with reason `DeAuthorized`. Setting the `StopTransactionOnInvalidId` configuration key to `false` turns this off.
- With `startOn: plug`, a transaction starts on plug-in using `startTag`. A tap can still stop it, under the same rules, as can unplugging.
- A connector is `Finishing` when its transaction has ended and the EV is still plugged in, however the transaction ended. An accepted tap or a remote start begins a new transaction there, with no need to unplug: the connector reports `Preparing` and then `Charging`, since OCPP 1.6 has no step from `Finishing` to `Charging`. A rejected tag leaves it `Finishing`.
- A new transaction on an EV that is already at its target starts all the same, and `onEvFull` applies at once: by default it ends with no energy delivered.
- When the EV reaches its target, `onEvFull` decides between `StopTransaction` followed by `Finishing`, and `SuspendedEV` with the transaction left open.
- `StopTransaction` reasons: `Local` for a tap, `EVDisconnected` for an unplug, `Remote` for a remote stop, `DeAuthorized` as above, `SoftReset`, `HardReset` and `Reboot` for those, `UnlockCommand` for `UnlockConnector`, `PowerLoss` after a power cut, and `Other` when the EV is full or the connector faults, since OCPP 1.6 has no reason for either.
- When a charging profile limits a connector to zero during a transaction, the connector reports `SuspendedEVSE` and returns to `Charging` when the limit rises. The transaction stays open.

### Metering

- Each connector has a meter register in Wh that only counts up. It is not reset between transactions; `meterStart` and `meterStop` are readings of it.
- During a transaction the charger sends `MeterValues` every `MeterValueSampleInterval` seconds (default 60) with the measurands listed in the `MeterValuesSampledData` configuration key.
- Supported measurands: `Energy.Active.Import.Register` (Wh), `Power.Active.Import` (W), `SoC` (Percent), and `Power.Offered` (W) and `Current.Offered` (A), which report what the connector offers the EV under the charging profiles in force. The default is the first two. A model of a DC charger adds `SoC` in its Chargerfile `config`; an AC charger cannot know the state of charge.
- `StopTransaction` carries `transactionData` with the measurands in the `StopTxnSampledData` key, sampled at the start and the end of the transaction. The default is `Energy.Active.Import.Register`. Setting the key to empty sends no `transactionData`.
- Meter readings are saved to disk every `MeterSaveInterval` seconds (default 5) during a transaction. This is plugyard's own configuration key, set like any other in a Chargerfile's `config`. It bounds the energy a power cut loses from the `meterStop` reported afterwards, as on a real meter.

### CSMS-initiated operations

Handled automatically with no user action:

| Request | What the charger does |
|---|---|
| `RemoteStartTransaction` | Behaves as after an accepted tap: starts a transaction if an EV is plugged in, and otherwise reports `Preparing` and waits `ConnectionTimeOut` seconds for one. With no connector named it picks one with an EV waiting, then one that is `Finishing`, then the lowest-numbered free one. `Rejected` when no connector is free; one that is `Finishing` is free. With `AuthorizeRemoteTxRequests` set to `true` (default `false`) it sends `Authorize` for the tag first |
| `RemoteStopTransaction` | Stops the transaction with reason `Remote`. `Rejected` for a transaction ID it does not have |
| `Reset` | Ends transactions with `StopTransaction`, reason `SoftReset` or `HardReset`, then reboots: it is away for about a second and boots again. A soft reset closes the connection properly; a hard reset drops it |
| `GetConfiguration` | Reports the keys asked for, or all of them, each with its value and whether it is read-only |
| `ChangeConfiguration` | Changes a key at once and saves it. `Rejected` for a read-only key or a value that does not fit, such as text for a number. `NotSupported` for a key the charger does not have |
| `ChangeAvailability` | Makes a connector `Unavailable` or available again. A connector in a transaction answers `Scheduled` and becomes `Unavailable` when the transaction ends. Connector 0 means the charger and all its connectors. Availability is saved |
| `UnlockConnector` | Ends a transaction on the connector with reason `UnlockCommand` and answers `Unlocked`. The EV stays until it is unplugged |
| `ClearCache` | Empties the authorization cache |
| `DataTransfer` | `Accepted` for the vendor IDs the Chargerfile lists under `dataTransfer`, `UnknownVendorId` otherwise |
| `TriggerMessage` | Sends the `BootNotification`, `Heartbeat`, `MeterValues` or `StatusNotification` asked for. For `FirmwareStatusNotification` and `DiagnosticsStatusNotification` it sends where the update or the upload stands, or `Idle` |
| `UpdateFirmware`, `GetDiagnostics` | Described under Firmware and diagnostics |

- Requests of Local Auth List Management and Reservation are answered with the error `NotSupported`.
- `SetChargingProfile`, `ClearChargingProfile` and `GetCompositeSchedule` are handled as well, as described under Charging profiles.
- A request whose payload cannot be read is answered with the error `FormationViolation`, and one the charger does not know with `NotImplemented`.
- Read-only keys: `NumberOfConnectors`, `SupportedFeatureProfiles`, `GetConfigurationMaxKeys`, and the four that describe charging profiles (below).

### Charging profiles

A charging profile is a limit over time that the CSMS puts on charging. The charger follows OCPP 1.6 Smart Charging.

| Request | What the charger does |
|---|---|
| `SetChargingProfile` | Installs the profile, in place of one with the same ID or with the same purpose and stack level on that connector. The new limit applies at once and is saved |
| `ClearChargingProfile` | Removes the profiles that match every field given: ID, connector, purpose, stack level. With no field it removes all. `Unknown` when none matched |
| `GetCompositeSchedule` | Reports the limit that results from all profiles, from now for the duration asked (at most 7 days), in W or in A (W when no unit is asked). Connector 0 reports the charger as a whole |
| `RemoteStartTransaction` with a `chargingProfile` | The profile must be a `TxProfile`; it is installed when the transaction starts |

Which profile decides:

- `TxProfile` is for one running transaction on one connector. It is `Rejected` when the connector has no transaction, or when it names another transaction. It is removed when its transaction ends.
- `TxDefaultProfile` applies to every transaction. One installed on a connector overrides one installed on connector 0.
- A `TxProfile` overrides a `TxDefaultProfile`.
- Within one purpose on one connector, the profile with the highest stack level that has something to say at that moment decides, whatever its limit. A profile says nothing before its start, after its `duration`, and outside `validFrom` to `validTo`.
- `ChargePointMaxProfile` is installed on connector 0 only and caps the result. Its limit is shared equally by the connectors that are charging: 30 kW with two connectors charging gives each 15 kW. A share an EV does not use is not passed on to the other connectors.
- The connector's `maxPower` and the EV still apply: delivered power is the lowest of all of them.

Schedules:

- All three kinds are supported. `Absolute` starts at `startSchedule`, or when the profile is installed if it has none. `Relative` starts when the transaction starts. `Recurring` repeats from `startSchedule` every day or every week.
- Schedules follow the real clock, whatever `--speed` is, like every other protocol timer. A period that starts at 3600 starts one real hour in.
- A limit in W is used as given. A limit in A is turned into power with the profile's `numberPhases` when it gives one, otherwise the Chargerfile's `phases`, and the Chargerfile's `voltage`.
- Only a limit of zero suspends charging. `minChargingRate` is accepted and ignored.

Limits, readable by the CSMS as read-only configuration keys:

| Key | Value |
|---|---|
| `ChargeProfileMaxStackLevel` | 10 |
| `ChargingScheduleAllowedChargingRateUnit` | `Current,Power` |
| `ChargingScheduleMaxPeriods` | 24 |
| `MaxChargingProfilesInstalled` | 20 |

A profile outside these limits, or one that is malformed (no periods, a first period that does not start at 0, periods out of order, a negative limit, an unknown purpose, kind or unit, a `Recurring` profile with no `recurrencyKind`), is answered `Rejected`, and the log says why.

Each time the limit on a transaction changes, the charger's log says so, for example `connector 1: a charging profile limits charging to 11.04kW`.

### Firmware and diagnostics

The charger follows OCPP 1.6 Firmware Management. A firmware update is acted out: the charger reports the statuses a real charger reports and fetches no file, so its `location` can be any address. A diagnostics upload is real: the charger sends its log to the `location`.

**`UpdateFirmware`.** The charger answers, waits for the `retrieveDate`, and then reports with `FirmwareStatusNotification`:

| Status | When | How long |
|---|---|---|
| `Downloading` | At the `retrieveDate`, or at once if that has passed | 10 seconds |
| `Downloaded` | The download is done | Until no transaction is running |
| `Installing` | No transaction is running | 10 seconds |
| `Installed` | The installation is done | |

- After `Installed` the charger reboots: it closes the connection, is away for about a second, and sends `BootNotification` with the new firmware version.
- The new version is the file name of the `location` without its extension: `ftp://host/fw-2.4.1.bin` gives `fw-2.4.1`. It is saved with the charger and shown by `inspect`. From then on it takes precedence over the `firmware` of the Chargerfile, until the charger is removed or reset.
- From `Downloaded` until the installation ends, every connector without a transaction reports `Unavailable` and starts nothing. Transactions that are running are left to end, as the standard recommends.
- The two 10-second steps are shortened by the site's `--speed`. The `retrieveDate` follows the real clock, as charging profile schedules do.
- A new `UpdateFirmware` replaces one that is waiting or downloading. During an installation it is ignored.
- An update does not survive `stop` or `kill`: the charger comes back with the firmware it had.

**`GetDiagnostics`.** The charger answers with a file name, `diagnostics-<charge point ID>-<time>.log`, reports `Uploading`, uploads the file to the `location`, and reports `Uploaded` as soon as the server has taken it, all with `DiagnosticsStatusNotification`.

- The file is the charger's own log: the lines `plugyard logs <charger>` shows, events and OCPP messages, as far back as the site's log files go. `startTime` and `stopTime` choose the lines; without them the file holds everything.
- The `location` is a directory, and the file is put into it under the name the charger answered with:

  | Location | How the file is sent |
  |---|---|
  | `http://`, `https://` | `PUT <location>/<file name>`. Any `2xx` answer counts as taken |
  | `ftp://` | Passive mode: log in, change to the directory, `STOR <file name>` |
  | `ftps://` | As `ftp://`, encrypted. The connection starts as FTP and turns encryption on with `AUTH TLS` (explicit FTPS), on port 21 unless the location names one. With port 990 in the location it is encrypted from the first byte (implicit FTPS). The connection for the file is encrypted too |
  | `sftp://` | Over SSH, on port 22 unless the location names one. The path is taken from the root of the server, and the directory has to exist |

- A user name and a password in the location (`ftp://user:password@host/dir/`) are used to log in: as HTTP Basic authentication, or as the FTP or SFTP login. Without them FTP and SFTP log in as `anonymous`. SFTP logs in with a password only, since a location cannot carry a key.
- The certificate of an `https://` or `ftps://` server is checked. `uploads: { insecureSkipVerify: true }` in the site file, or `--upload-insecure-skip-verify` on `run`, turns the check off, for a server with a self-signed certificate. An upload that fails on the certificate says so and names the setting.
- The key of an `sftp://` server is taken as it is, as most chargers do: SSH has no certificate authorities, and a `GetDiagnostics` request has no place for a key. The charger's log shows the key's fingerprint for every upload. The password is left out of the charger's events; it does show in the logged OCPP message that carried it.
- When the upload cannot be made, the charger reports `UploadFailed`, and its log says why: the server cannot be reached, it refused the login or the file, the location uses something other than `http`, `https`, `ftp`, `ftps` or `sftp`, or the location is a machine the charger may not upload to. One attempt may take 60 seconds.

**Where a charger may upload to.** The CSMS chooses the `location`. Without a limit, a CSMS that is hostile or has been broken into could make the machine plugyard runs on connect to any address that machine can reach, such as the internal services of a network.

| Location | Allowed |
|---|---|
| The machine the CSMS is on | Always. This gives the CSMS no reach it did not have |
| A host listed in `uploads.allow` of the site file, or given with `--allow-upload` to `run` | Yes |
| A link-local address (`169.254.0.0/16`, `fe80::/10`), where the metadata of cloud machines is served, and `fd00:ec2::/32` | Never, whatever is listed |
| Anything else | No |

- "The machine the CSMS is on" is the address the charger's connection to the CSMS goes to. When the CSMS is on a loopback address, every loopback address counts: `localhost`, `127.0.0.1` and `::1`. So a CSMS and a file server on the developer's own machine need no setting.
- The check is made on the address that is connected to, after the host name has been looked up, and on every connection of the upload: the one an HTTP redirect leads to, and the second connection an FTP server asks for. A name that points somewhere else at the moment of the upload does not get around it.
- A host allowed by name is allowed under that name only, and an address under that address only.
- A refused upload connects to nothing. The charger's log names the host and the line that would allow it.
- Uploads do not go through an HTTP proxy set in the environment, since the check has to see where the upload really goes.
- When the charger reaches its CSMS through a proxy, the proxy's machine is the one that counts.
- A charger started with `run --rm`, which keeps no log, and a period with no lines, give a file of one line that says so.

**Failures.** `exec <charger> fail <step>` makes the next step of that kind fail:

| Step | The charger reports | Then |
|---|---|---|
| `download` | `DownloadFailed` in place of `Downloaded` | Keeps its firmware, does not reboot |
| `install` | `InstallationFailed` in place of `Installed` | Keeps its firmware, does not reboot, connectors are available again |
| `upload` | `UploadFailed` in place of `Uploaded` | Nothing is sent to the `location` |

- A failing download or upload is tried again as often as the `retries` of the request say, `retryInterval` seconds apart, and the failure is reported once, after the last attempt. With no `retries` it is tried once. The log shows every attempt.
- `fail` stays set until that step has failed, or the charger is powered off. It then is spent: the next update goes through.
- A request with no `location` is answered with the error `PropertyConstraintViolation`.

### Failures

Failures are caused on purpose, with actions, so that a CSMS can be tested against them.

**Faults.** `exec <charger> fault <errorCode>` makes a connector report `Faulted` with one of the OCPP 1.6 error codes: `ConnectorLockFailure`, `EVCommunicationError`, `GroundFailure`, `HighTemperature`, `InternalError`, `LocalListConflict`, `OtherError`, `OverCurrentFailure`, `OverVoltage`, `PowerMeterFailure`, `PowerSwitchFailure`, `ReaderFailure`, `ResetFailure`, `UnderVoltage`, `WeakSignal`.

- A transaction on the connector ends with reason `Other`.
- While faulted, an EV can still be plugged in or unplugged, but a tap and a remote start are refused.
- `clear` returns the connector to `Available`, or to `Finishing` if an EV is still plugged in.
- With `--connector 0` the fault is the charger's: it is reported for connector 0, every transaction ends, and nothing starts on any connector until it is cleared.
- A fault is saved, and is reported again when the charger next boots.

**Network loss.** The charger cannot tell why the CSMS is out of reach, so it behaves the same whatever the cause: charging continues, transaction messages queue in order, and after reconnecting it boots and sends them before anything new.

- `exec <charger> offline` takes the connection down until `online`. `--for 30s` brings it back after that time, which is real time, whatever the `--speed` of the site.
- When the CSMS goes away by itself, the charger retries by itself, as described under Connection.
- A tap while offline is refused, since the tag cannot be authorized, unless one of two standard keys allows it. `AllowOfflineTxForUnknownId: true` accepts any tag. `AuthorizationCacheEnabled: true` has the charger remember what the CSMS said about each tag and accept the ones it accepted; the cache is saved, and `ClearCache` empties it. Both are `false` by default.
- Stopping a transaction while offline always works.
- `offline` is not remembered across `stop` and `start`. A charger that is to stay away says so in its declared state: `state: { connection: offline }` (section 6).

**Answers that are errors.** A `StartTransaction`, `StopTransaction` or `MeterValues` that the CSMS answers with an error is sent again after `TransactionMessageRetryInterval` seconds (default 60) times the number of attempts so far, up to `TransactionMessageAttempts` attempts (default 3), and is then dropped. Messages behind it wait, so their order is kept. An answer to `StartTransaction` that cannot be read counts as an error. Other messages are not sent again.

**Reboot.** `exec <charger> reboot` does what a soft reset does, with reason `Reboot`.

### Time

`--speed N` (or `speed:` in the site file) multiplies the rate of EV charging and energy accumulation. Protocol timers such as heartbeat and meter value intervals run in real time, as do charging schedules, and message timestamps use the real clock. At `--speed 60` an hour of charging takes a minute.

## 9. Running and saved state

- There is no daemon. The `site up` process owns its chargers. Other commands find that process and talk to it over a local socket: a Unix domain socket file per running site, in the data directory (see ADR 0004).
- A site does not start by itself after a reboot; run `plugyard site up -d` again.
- `site stop`, `site start` and `site ls -a` depend on saved state: a stopped site has no process, so its chargers exist only on disk.
- The implicit site of `plugyard run` is saved in the same way, unless `--rm` is given.
- State is stored in one SQLite database per site, in a per-user data directory: `%LOCALAPPDATA%\plugyard` on Windows, `~/.local/share/plugyard` on Linux, `~/Library/Application Support/plugyard` on macOS.
- The database is accessed through a pure-Go SQLite driver so that all platforms build without a C compiler.
- The `PLUGYARD_DATA_DIR` environment variable overrides the data directory. Socket paths are limited to about 100 characters, so a data directory with a very long path is refused with a message saying so.

Scale:

- One site of 2,000 chargers is a tested size. Each charger is a few lightweight threads of one process and one connection.
- Measured with the project's scale test, in which the site and a test CSMS share one process: 2,000 chargers were all online after about 1 second, each completed a transaction with meter values within 3 seconds, and all shut down in an orderly way in under half a second. The process had taken about 300 MB from the system on Linux and under 400 MB on Windows.
- The same test runs with 200 chargers on every change, and with 2,000 before a release (`PLUGYARD_SCALE=2000`).

Saved per charger:

- Its name
- A snapshot of the model and site-file settings it was created from, with its declared state
- Configuration keys and their current values
- Installed charging profiles
- Meter reading per connector
- Connector status and the open transaction, if any
- Messages queued while offline
- Authorization cache, if enabled
- Faults, and connectors the CSMS made unavailable

A CSMS password is never saved. It is read again from where it came from whenever a site starts: the site file for a site, and the `PLUGYARD_CSMS_PASSWORD` environment variable for a charger started with `plugyard run`.

Write timing: configuration, charging profile and transaction changes, and messages queued for the CSMS, are written immediately. Meter readings are written every `MeterSaveInterval` seconds and at each transaction event. Chargers that save at the same moment are written together.

When files and saved state disagree, `site up` and `run` settle it:

| Edit | Effect |
|---|---|
| Vendor, model name or firmware version | Applied. The next `BootNotification` carries the new values, as after a firmware update. A firmware version installed by `UpdateFirmware` takes precedence over the file |
| Connector type or maximum power, behavior settings, CSMS URL, default EV | Applied |
| A configuration key | The file wins, unless the CSMS changed that key; then saved state wins |
| Declared state | Applied, with no confirmation. The file wins over saved state at this and every later power-on, until the next `site up` reads the file again |
| The number of connectors | The charger's saved state is reset, after confirmation. It keeps its name |
| A charger removed from the site file | The charger and its saved state are deleted, after confirmation |

- Confirmation: the affected chargers are listed and `Reset these 3 chargers? [y/N]` is asked. Anything but yes stops the command with nothing changed and nothing started.
- `-y, --yes` skips the question. Without a terminal to ask on, the command fails with exit code `1` and names that flag.
- A charger that has never run has no state to lose and needs no confirmation.
- `--reset` on `site down` returns the whole site to the files.

## 10. Scenarios

```yaml
name: commute
steps:
  - at: 0s
    charger: "FAST-*"
    do: plug
    ev: longrange
  - at: 5s
    charger: "FAST-*"
    do: tap
    tag: 04A1B2C3
  - at: 10m
    charger: FAST-003
    do: fault
    code: GroundFailure
  - at: 30m
    charger: "FAST-*"
    do: unplug
```

A scenario is a name and a list of steps. A step has three required fields:

| Field | Meaning |
|---|---|
| `at` | When the step runs, counted from the start of the run: `0s`, `30s`, `10m`, `1h30m` |
| `charger` | A charge point ID, a name, or a pattern, as for `exec` |
| `do` | The action |

What a step can do, with the options that go with it:

| `do` | Options | Same as |
|---|---|---|
| `plug` | `connector`, `ev`, `battery`, `soc`, `maxPower`, `targetSoc`, `curve` | `exec <charger> plug` |
| `unplug` | `connector` | `exec <charger> unplug` |
| `tap` | `tag` (required), `connector` | `exec <charger> tap <tag>` |
| `fault` | `code` (required), `info`, `vendorCode`, `connector` (0 is the whole charger) | `exec <charger> fault <code>` |
| `clear` | `connector` | `exec <charger> clear` |
| `offline` | `for`, in scenario time | `exec <charger> offline`, where `--for` is real time |
| `online`, `reboot` | | `exec <charger> online`, `reboot` |
| `fail` | `step` (required): `download`, `install` or `upload` | `exec <charger> fail <step>` |
| `stop`, `start`, `restart`, `kill` | | `plugyard stop <charger>` and so on |

- Steps are written in the order they run. A step whose `at` is earlier than the step above it is an error. Steps with the same `at` run one after the other, in file order.
- Unknown fields, and an option on an action it does not apply to, are errors. Every problem in the file is reported at once.
- There are no assertions, conditions or loops.

### Running a scenario

- A scenario runs against a site that is already up: the one named with `-s`, or the only one running. With several running and no `-s`, the command stops and says so. Patterns match only chargers of that site.
- Every time in a scenario is scenario time: a step's `at`, and the `for` of an `offline` step. A scenario runs at the speed of its site. Against a site at `--speed 60`, a step at `30m` runs after 30 seconds, when the EVs have charged for 30 minutes, and `for: 2m` keeps the charger away for two real seconds. `--speed N` on `scenario run` gives the run a speed of its own (see ADR 0008).
- An `offline` step prints both times when the speed is not 1: `offline --for 2m0s (2s at speed 60)`. A real time of a millisecond or more is rounded to the millisecond. At speed 1 the time is sent as written.
- In an outage that short, a charger queues only what the scenario makes it send: `MeterValues` and `Heartbeat` run on real-time timers. For an outage that lasts a real time, such as one the CSMS must notice after five minutes, run at speed 1, or use `exec <charger> offline --for` beside the run.
- Each step prints what `exec` prints: one line per charger, and a count when there are several. `--format json` prints one object per charger and step, with `step`, `at` and `do` added to the fields `exec` gives.
- A step that fails on a charger, or matches no charger, does not stop the run. Every failure is reported. `--fail-fast` stops at the first step that failed.
- As for `exec`, a tag the CSMS rejects is an outcome and not a failure.
- Ctrl+C stops the run. The site keeps running.
- Stopping the last powered charger of a site ends the site's process, as `plugyard stop` does. A later `start` step brings the site back in the background.

Exit codes: `0` when every step ran and every action succeeded; `1` when an action failed, a step matched no charger, or the run was interrupted; `2` when the file has a mistake.

### Trying steps while writing one

- `plugyard scenario validate <file>` checks the file and prints the plan: each step's number, time, charger and action. Nothing is sent to a charger. When a site is named with `-s`, or exactly one is running, the plan also says how many of its chargers each step would act on, and marks a step that matches none.
- `plugyard scenario run <file> --step N` runs one step now.
- `--from N` and `--to N` run a part of the scenario. The part starts at once, and keeps the gaps between its own steps.

## 11. Logs

- Two kinds of timestamped line per charger: events (connected, transaction started, fault) and raw OCPP messages in each direction.
- Each line is a timestamp, the charge point ID, a marker and the text. The marker is `|` for an event, `>` for a message sent to the CSMS and `<` for a message received from it.

```
2026-01-02T15:04:05.000+01:00 CP-1 | accepted by the CSMS
2026-01-02T15:04:05.010+01:00 CP-1 > [2,"7","Heartbeat",{}]
2026-01-02T15:04:05.020+01:00 CP-1 < [3,"7",{"currentTime":"2026-01-02T14:04:05.015Z"}]
```

Where the log is kept:

- One log per site, in plain files beside the site's database. It is not stored in the database.
- It is written whether the site runs in the foreground or the background. A foreground site prints the same lines to its terminal as well.
- The log is capped. The current file is rotated when it reaches `maxSize`, and at most `files` files are kept, so the log holds the newest `maxSize` x `files` at most: 30 MB by default. The site file's `log` setting changes the cap.
- The log outlives a stop, and is deleted with the saved state by `site down --reset` and by `rm` of a charger started with `plugyard run`.
- A charger started with `run --rm` keeps no log.

Reading it:

| Command | Shows |
|---|---|
| `plugyard logs <charger>` | The lines of one charger, or of every charger matching a pattern |
| `plugyard site logs` | The lines of every charger of a site |

| Flag | Effect |
|---|---|
| `-f`, `--follow` | Keep showing new lines. Following ends on Ctrl+C, or by itself when the site stops, as `docker logs -f` does |
| `--events` | Only events, no OCPP messages |
| `--since <when>` | Only lines from that moment on: how long ago (`10m`, `2h`) or a time (`2026-01-02T15:04:05Z`) |
| `--tail N` | Only the last N lines. The default is everything still in the files |
| `--format json` | One object per line |

- Both commands work while the chargers are stopped.
- A JSON line has `time`, `chargePointId`, `kind` (`event`, `sent` or `received`) and `text`. A line that is an OCPP message also has `message`, the message as JSON, so that a script can read the action or the payload without parsing text.

```json
{"time":"2026-01-02T15:04:05.010+01:00","chargePointId":"CP-1","kind":"sent","text":"[2,\"7\",\"Heartbeat\",{}]","message":[2,"7","Heartbeat",{}]}
```

## 12. Getting started, editor support and AI agents

`plugyard init` writes three files into the current directory, or the one named: `site.yaml` with three chargers, the `Chargerfile` they are made from, and `scenario.yaml`, a sample scenario.

- `plugyard site up` works on them as they are. The chargers connect to the `CSMS_URL` environment variable, or to `ws://localhost:9000/ocpp` when it is not set. `--csms <url>` writes an address into the site file instead.
- A file that exists is never overwritten: `init` stops and writes nothing. `--force` overwrites.
- It ends by printing the next commands to run.

### AI agents

AI agents, such as coding assistants that run commands for a user, are a third kind of user. Three things are made for an agent that has a shell where plugyard runs:

| What | Where | For |
|---|---|---|
| The guide | `llms.txt` at the root of the repository, and built into the program: `plugyard agents` prints it | An agent that has plugyard and needs to know how to drive it: the rules that keep a session from hanging, a complete first session, how to read the output |
| The pointer to it | The last line of `plugyard --help`: `AI agents: run "plugyard agents" for a guide written for you.` | An agent reads the help first |
| The skill | `skills/plugyard/SKILL.md` in the repository | An agent working in a project that uses plugyard, which would not know of plugyard otherwise |

- The guide printed by `plugyard agents` is the one of the installed version. Its first session is run by the tests as it is written, and every command it names is checked to exist.
- The guide names every command, every action, every setting of the three file types and every tool the API serves over MCP, and says what plugyard does not do and how to run it in a container. The tests fail when one is added and the guide is not told, since what the guide does not name an agent does not look for.
- The skill is short on purpose: its description, which is what makes an agent pick it up, the few rules that do not change between versions, and the instruction to run `plugyard agents`.
- `plugyard init --agents` installs the skill into the project with the skills tool (`https://skills.sh`), by running `npx skills add Amraa1/plugyard` there. That tool knows where each agent keeps its skills and asks which agents to install for.
  - It needs Node.js for `npx`. When `npx` is not found, nothing is written, and the message says to install Node.js or to copy the skill by hand.
  - The command is printed before it runs. Without a terminal it is run as `npx --yes skills add Amraa1/plugyard --yes`, so that nothing asks.
  - In a folder that already has the starter files, `--agents` installs only the skill and leaves the files alone.
  - plugyard does not write the skill itself.
- Plain `plugyard init` names `plugyard init --agents` among the next commands.
- An agent with no shell where plugyard runs, such as one on a developer's machine while plugyard is on a staging server, uses the tools the API serves over MCP (section 14).

JSON schemas for the three file types are in the repository under `schemas/`, and inside the program:

| File type | Schema | Printed by |
|---|---|---|
| Chargerfile | `schemas/chargerfile.schema.json` | `plugyard schema chargerfile` |
| Site file | `schemas/site.schema.json` | `plugyard schema site` |
| Scenario file | `schemas/scenario.schema.json` | `plugyard schema scenario` |

- An editor with YAML schema support completes settings and marks mistakes when a file's first line names its schema: `# yaml-language-server: $schema=https://raw.githubusercontent.com/Amraa1/plugyard/main/schemas/site.schema.json`. The files `init` writes have that line.
- A schema catches misspelled settings, missing required ones and wrong values. The program's own check (`site validate`, `scenario validate`) remains the full one: it also reads the Chargerfiles a site refers to and resolves environment variables.
- The shipped examples and starter files are tested against the schemas, so the two cannot drift apart.

## 13. Distribution

Releases:

- A release is made by pushing a version tag such as `v0.2.0`. GoReleaser then builds the program for Windows, Linux and macOS, each for x86-64 and ARM64, and attaches the six archives and a `checksums.txt` to a GitHub Release.
- An archive is named `plugyard_<os>_<arch>`, with `.zip` for Windows and `.tar.gz` for the others, and no version in the name. The latest build of a system therefore has one fixed address, `https://github.com/Amraa1/plugyard/releases/latest/download/plugyard_linux_amd64.tar.gz`, which the install scripts and `plugyard update` rely on.
- Versions follow semantic versioning. Before 1.0, file formats and JSON output may change between minor versions. A tag with a suffix, such as `v0.2.0-beta.1`, is published as a pre-release.
- The programs are not code-signed.

Ways to install:

| Way | What it does |
|---|---|
| `install.ps1` (Windows), `install.sh` (Linux, macOS) | Downloads the latest release for the system, checks it against `checksums.txt`, and puts the program in the user's folder: `%LOCALAPPDATA%\Programs\plugyard`, which is added to the user's PATH, or `~/.local/bin`. Running it again updates |
| `go install github.com/Amraa1/plugyard@latest` | Builds from source with Go |
| By hand | Download an archive from the releases page and unpack it |
| `docker pull amraa1/plugyard` | The container image, on Docker Hub (see below) |

- `PLUGYARD_INSTALL_DIR` names another folder for the install scripts, and `PLUGYARD_NO_MODIFY_PATH` keeps `install.ps1` from changing the PATH.
- Homebrew, Scoop and winget later, on demand.

`plugyard update`:

- Finds the latest release, and stops if the installed version is that one or newer. `--check` only reports.
- A site started by one version cannot be controlled by another, since the channel between them may change. So sites and the API that are running are stopped for the update and started again on the new version:
  - `update` says what is running and how many transactions would end, and asks `Update and restart them? [y/N]`. `-y, --yes` skips the question. Without a terminal to ask on, it changes nothing, exits with code `1` and names that flag. With nothing running it does not ask.
  - The order is: download and check the new program, stop, replace, start again. A download that fails stops nothing.
  - A site is stopped in the orderly way: transactions end with `StopTransaction`. It comes back in the background with the chargers that were powered on, and those send `BootNotification` again. A charger that was stopped stays stopped. A site that ran in the foreground of another terminal comes back in the background.
  - The API comes back on the same address, with the same certificate.
  - If the program cannot be replaced, what was stopped is started again on the program that is still there.
  - If something does not come back, the update is done all the same. `update` says what did not start, with the command that starts it, and exits with code `1`.
- Downloads the archive for the system it runs on, checks it against the release's `checksums.txt`, and replaces the program. If anything fails, the program that was there stays.
- On Windows a running program cannot be deleted, so the old one is renamed and removed the next time plugyard starts.

The container image, `amraa1/plugyard` on Docker Hub:

- It holds the program and nothing else to speak of: no shell and no package manager. It runs as a user that is not root. It exists for `linux/amd64` and `linux/arm64`.
- A release `v0.2.0` is published as `amraa1/plugyard:0.2.0`, `:0.2` and `:latest`. A tag with a suffix, such as `v0.2.0-beta.1`, is published under its own version only. The release workflow builds and publishes it; nothing is published by hand.
- A container runs `plugyard`, with `site up` when nothing else is said. Whatever follows the image's name is given to `plugyard`, so every command and flag works as on the command line.

  | In the container | Holds | Give it |
  |---|---|---|
  | `/site` | The site file `site.yaml` and its Chargerfiles. plugyard starts in this folder | Your own folder, mounted there. Without one, a site of three chargers runs, which connects to `CSMS_URL` and uses `CSMS_PASSWORD` if set |
  | `/data` | Saved chargers, logs and API tokens | A volume. Without one, every new container makes new chargers with meters at zero |
  | Port `7070` | The API, when it is started | A published port, or a reverse proxy |

- What a container is told comes in three ways and no others: the mounted site file, environment variables that the site file reads, and the arguments after the image's name.
- `plugyard site up --api <address>` serves the API for as long as the site runs, so that one container can run both. It is the API of `plugyard api up`, with the same rules. `--api` and `-d` cannot be combined. `plugyard api down` stops that API and leaves the site.
- A token is made in a container of its own that shares the volume: `docker run --rm -v <volume>:/data amraa1/plugyard api token create <name>`. Until one exists, `--api 0.0.0.0:7070` is refused.
- `docker stop` shuts the site down in the orderly way. Transactions end with `StopTransaction` if Docker gives it the time; its default is ten seconds.
- In a container, `plugyard update` changes nothing and names the image to pull, and `plugyard uninstall` says to remove the container with Docker. The image tells plugyard that it is one with the `PLUGYARD_CONTAINER` environment variable.
- `examples/docker/compose.yaml` is a ready setup for a server: the site, the API, the volume, and a restart after a crash or a reboot of the server.
- The overview of the image on Docker Hub is `docker/README.md`. The release workflow sends it there with each release, with the release's version in place of the one its examples pin. A release stands when that step fails, and the step says so.

`plugyard uninstall`:

- Removes what plugyard has on the machine:

  | What | Removed |
  |---|---|
  | The program | Always: the file the command was started from, wherever it is |
  | The folder of the program | Only when it is named `plugyard` and holds nothing else. `%LOCALAPPDATA%\Programs\plugyard` is such a folder; `~/.local/bin` and Go's `bin` folder are not, and stay even when empty |
  | The entry on the user's PATH (Windows) | Only when the folder is removed |
  | Saved chargers and logs | The files plugyard wrote in its data directory, unless `--keep-data` is given. The directory itself goes when nothing else is in it |

- Shows this list with full paths and asks `Remove plugyard? [y/N]`. `-y, --yes` skips the question. Without a terminal to ask on, it removes nothing, exits with code `1` and names that flag.
- Stops sites and the API that are running, after the question has been answered with yes. The list it shows names them, and how many transactions would end.
- Takes the program away first. When that fails, for example for lack of permission, nothing else is removed, and the message names the file and says what to do.
- On Windows a running program cannot be deleted, so it is renamed to `plugyard.exe.old`, and a hidden command deletes it, and the folder, a few seconds after plugyard has ended.
- Does not undo `go install`'s module cache, and does not remove files that `plugyard init` wrote.
- Saved state and logs are kept.
- `PLUGYARD_RELEASES_URL` points the install scripts and `plugyard update` at another copy of the releases, such as a mirror inside a company network.
- The checksum guards against a damaged download. It does not prove who published the release.

## 14. The HTTP API

The API lets a caller with no shell on the machine act on chargers and read them (ADR 0006). The first users are the developers of apps and dashboards who test against a CSMS on a staging server, where plugyard runs.

### What it offers, and what it never does

It offers the actions of `exec` and what `ps` and `inspect` show. A call does what the command of the same name does, with the same code behind it.

It never does anything else. This is a rule, not a list of what is missing:

- nothing that takes a file path;
- no starting, stopping or removing of sites or chargers;
- no change to where a charger connects;
- no `update`, `uninstall`, `init` or `run`;
- no managing of its own tokens.

Those stay on the command line, because each is a way from "can call the API" to "owns the machine".

### The `api` suite

| Command | Effect |
|---|---|
| `plugyard api up [-d] [--listen <address>] [--tls-cert <file> --tls-key <file>]` | Start serving. In the foreground it prints every call until Ctrl+C; `-d` runs it in the background, as `site up -d` does. |
| `plugyard api down` | Stop serving. Sites and chargers are not touched. |
| `plugyard api status` | Whether it runs, at which address, since when, whether a call needs a token, and whether traffic is encrypted. |
| `plugyard api logs [--charger <ID>] [--tail N]` | The calls received, oldest first. |
| `plugyard api spec` | The description of the API, as an OpenAPI 3 document in JSON. |
| `plugyard api token create <name> [-s, --site <name>] [--read-only]` | Make a token and print it, once. |
| `plugyard api token ls` | List the tokens: name, site, what each can do, when it was made. Never the tokens themselves. |
| `plugyard api token rm <name>...` | Remove tokens. They stop working at once. |

- The API is a process of its own. It serves every charger on the machine, whichever site it belongs to. Sites know nothing of it, and it keeps running while they start and stop: a call sees the chargers there are at that moment, and an empty list when there are none.
- There is one API per user and machine. A second `api up` is refused and says where the first one listens.
- `status`, `logs` and `token ls` take `--format json`.
- `update` stops the API and starts it again on the new version, and `uninstall` stops it (section 13).

### Addresses

| Call | Does | As |
|---|---|---|
| `GET /v1/chargers` | Lists every charger, running or stopped | `ps -a --format json` |
| `GET /v1/chargers/{charger}` | Everything about one charger | `inspect --format json` |
| `GET /v1/chargers/{charger}/wait?state=<state>` | Answers once the chargers are in a state, or the time is up | `wait <charger> <state>` |
| `GET /v1/chargers/{charger}/logs` | The newest lines of the log of chargers | `logs <charger> --format json` |
| `POST /v1/chargers/{charger}/{action}` | Runs an action | `exec <charger> <action>` |
| `GET /v1/openapi.json` | The description of the API | `api spec` |
| `POST /mcp` | The same reading and actions as tools for AI agents (see below) | |

- `{charger}` is a charge point ID or a name. For an action, a wait and the log it may be a pattern, such as `FAST-*`.
- `{action}` is one of `plug`, `unplug`, `tap`, `fault`, `clear`, `offline`, `online`, `reboot` and `fail`.
- `?site=<name>` narrows a call to one site, as `-s` does. It is needed when two sites have a charger of the same charge point ID.
- The options of an action go in the body, as JSON: `{"tag": "04A1B2C3"}` for `tap`, `{"code": "GroundFailure", "info": "leak"}` for `fault`, `{"for": "30s"}` for `offline`, `{"step": "upload"}` for `fail`, `{"connector": 2}` where a connector is meant, and `soc`, `battery`, `maxPower`, `targetSoc`, `curve` and `ev` for `plug`, written as on the command line. An action without options takes `{}`. An option the action does not take is an error.
- A list is answered as `{"chargers": [...]}`, an action as `{"results": [...]}` with one result per charger, each as `exec --format json` prints it, and a failure as `{"error": "..."}`.
- **A wait** takes the state as `?state=`, a connector status or a connection state as `plugyard wait` takes it (section 4), and may take `connector=<n>` and `timeout=<time>`.
  - It is answered as `{"reached": true, "chargers": [...]}`. Each charger says its site, its charge point ID, the state it was in when the wait ended, whether that is the state waited for, and the connector for a connector status.
  - A state that did not come in time is an answer and not a failure: the status is `200` and `reached` is `false`. This is how a call that waits is answered elsewhere too, and it lets a caller tell "not yet" from a mistake in the call.
  - The timeout is `30s` unless given, and `5m` at most. To wait longer, the caller calls again. A reverse proxy in front may end a call sooner than that.
  - A wait that is still going on when the API stops is answered with `503`.
- **The log** is answered as `{"lines": [...]}`, oldest first, each line as `logs --format json` prints it. It takes `since=<how long ago or a time>`, `events=true` and `tail=<n>`, as the flags of `plugyard logs` (section 11).
  - It answers with the newest `200` lines unless `tail` says otherwise, and with `5000` at most, so that one call never answers with a whole log.
  - A charger that has not run yet has no lines: the answer is an empty list.
  - It does not follow the log. A caller that wants what comes next calls again with `since`.
- A parameter in the address that the call does not take is a mistake, as an option an action does not take is.
- Waiting and reading the log are reading: a read-only token can do both.

| Status | Meaning |
|---|---|
| `200` | Done. For an action: carried out on every charger it named |
| `400` | The call has a mistake: an option or parameter that is missing, wrong or not taken, or a body that is not JSON |
| `401` | A token is needed, and none or a wrong one was sent |
| `403` | The token may not do this: it is read-only, or it is limited to a site and asked for something outside it. Also a call a browser sent from another website |
| `404` | No such charger, site, action or address. A token limited to a site is answered with `403` instead |
| `409` | The action could not be carried out on at least one charger, with the results; or the charger is stopped, or its charge point ID is in two sites |
| `415` | An action whose body is not declared as JSON |
| `503` | A wait was still going on when the API stopped |

- As for `exec`, a tag the CSMS rejects is an outcome and not a failure: the call is answered with `200`, and the result says what the CSMS said.
- The description that `api spec` prints is held to the server by the tests: every address, action and option in the one is in the other.

### Who may call

- **By default the API listens on `127.0.0.1:7070`**, which only the machine itself can reach.
- **Listening on any other address is refused while no token exists.**
- **Without any token, calls from the machine itself need none.** As soon as one token exists, every call needs one, also from the machine itself. When the last token is removed from an API that others can reach, nobody gets in.
- **A token** is made with `plugyard api token create <name>` and sent as `Authorization: Bearer <token>`.
  - It is shown once. plugyard keeps a hash, in a file in its data directory that only the user can read.
  - It has a name, which the log of calls shows. Each team or tool gets its own.
  - `--site` limits it to the chargers of one site: it sees and reaches no others. A charger it cannot reach is answered with `403` and the name of the site it is limited to, whether that charger is in another site or does not exist. The answer is the same for both, so it tells nothing about other sites.
  - `--read-only` makes one that can look and not act.
  - Removing it takes effect at once.
- **A web page in a browser cannot act through the API.** A page a developer has open can send requests to their own machine, and an API there with no token would let any website unplug a charger. Three things prevent it: a call to an API that listens on the machine only must name that machine as its host; a call must not come from a page of another origin; and an action must declare its body as JSON, which a page cannot do across origins without first asking a question the API never answers with yes.
- **Encryption**: `--tls-cert` and `--tls-key` serve HTTPS. Without them a token travels unencrypted, which is safe on the machine itself or behind a reverse proxy that adds encryption. `api up` prints a warning when it listens beyond the machine without them.
- **`api up` prints a warning when it runs as root.** Run it as a user of its own, so that a flaw in it reaches no further than plugyard's data.

### The log of calls

Every call is recorded, also the ones turned away:

```
2026-11-24T16:11:26.707Z mobile-team 127.0.0.1 200 plug CP-1: EV plugged in (60 kWh, 20%)
2026-11-24T16:11:26.765Z dashboard 127.0.0.1 403 plug CP-2: token dashboard is read-only: it can look at chargers and not act on them
```

- A line has the time, the name of the token, the address the call came from, the status it was answered with, the action, the chargers it was about, and how it ended. `local` stands for a call that needed no token, and `-` for one turned away before its token was known.
- `plugyard api logs --charger CP-1` shows the calls about one charger, which answers who acted on it. For a pattern, the line names the chargers the action ran on.
- A token is never recorded.
- Behind a reverse proxy the address is the proxy's.
- The log is kept in the data directory, capped like a site's log, and outlives the API.
- A wait is recorded when it ends, with whether the state was reached: `wait CP-1: Charging`, or `wait CP-1: not Charging after 30s`.
- What the CSMS wrote, such as a rejected tag's status, can appear in an outcome, and the log of a charger holds the messages the CSMS sent. It is data, never an instruction (section 12).

### Tools for AI agents (MCP)

An AI agent on another machine reaches the chargers through tools, over the Model Context Protocol (MCP), at the address of the API followed by `/mcp` (ADR 0007). The first users are developers who work through a coding agent and test against a CSMS on a staging server.

In Claude Code it is added with:

```sh
claude mcp add --transport http plugyard https://plugyard.example.com/mcp --header "Authorization: Bearer <token>"
```

- **It is part of the API**: `plugyard api up` and `site up --api` serve it, with no switch of its own. `api up` and `api status` print its address.
- **Every rule of "Who may call" holds.** It needs a token when a call under `/v1` does, sent the same way. It is closed to web pages in a browser.
- **There are no sessions.** Every request stands alone and carries its token, so nothing is kept between requests and the API can restart without the agent noticing. A `GET` is answered with `405`.

| Tool | Does | As |
|---|---|---|
| `list_chargers` | Lists every charger, running or stopped | `GET /v1/chargers` |
| `inspect_charger` | Everything about one charger | `GET /v1/chargers/{charger}` |
| `wait_for_state` | Answers once the chargers are in a state, or the time is up | `GET /v1/chargers/{charger}/wait` |
| `read_logs` | The newest lines of the log of chargers | `GET /v1/chargers/{charger}/logs` |
| `plug`, `unplug`, `tap`, `fault`, `reboot` | Run the action of the same name | `POST /v1/chargers/{charger}/{action}` |
| `clear_fault`, `go_offline`, `go_online`, `fail_next` | Run `clear`, `offline`, `online` and `fail` | `POST /v1/chargers/{charger}/{action}` |

- **One tool per action**, so that an agent's client can show each for what it is, and ask its user before one and not another.
- **Each tool is marked as reading or acting.** `unplug`, `fault`, `go_offline` and `reboot` are also marked as able to end or interrupt a transaction.
- **A read-only token is offered the four reading tools and no others.** Its agent never sees an action.
- **The arguments are those of the call**: `charger`, `site`, and the options of the action or the parameters of the wait or the log, with the same names. An argument a tool does not take is a mistake.
- **A tool answers with what the call answers with**: the same fields, as JSON.
  - `read_logs` answers with the newest `50` lines unless `tail` says otherwise, fewer than the call, since an agent reads every line it is given.
  - A state that did not come in time is an answer with `"reached": false`, not a failure.
  - A failure is told in the words the call would use, marked as a failure of the tool, so that the agent can put its call right. An action that could not be carried out on every charger is such a failure, and has the results.
- **What the CSMS wrote is data.** The instructions the server gives an agent when it connects say so, and so does the description of every tool whose answer can hold such text: `list_chargers`, `inspect_charger`, `read_logs` and `tap`.
- **Each call of a tool is a line in the log of calls**, with the action as a call under `/v1` names it: `plug CP-1: EV plugged in`. A call that reached no tool, because no tool has that name for this token or because its arguments are wrong, is recorded with `400` and the name it used. The requests the protocol makes for itself are not recorded, except those turned away.
- **Tools cannot start, stop, add or remove chargers**, or do anything else the API does not do.
- The tools are tested with a client of the protocol against real simulated chargers.
