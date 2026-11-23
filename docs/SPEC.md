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

### Scale

One site must run hundreds to a few thousand chargers comfortably on a developer laptop.

## 2. Scope

### In V1

- OCPP 1.6J over WebSocket
- Feature profiles: Core (complete), Remote Trigger, Smart Charging
- `ws://`, and `wss://` with HTTP Basic Auth (security profile 2), with an option to skip certificate verification
- A minimal EV model that produces realistic meter values
- Chargers defined in files, driven by commands and by scenarios
- Charger state saved to disk, surviving stops, terminal exits and machine shutdowns

### Not in V1

- OCPP 2.0.1 (the protocol sits behind a seam so it can be added)
- Firmware Management, Local Auth List and Reservation profiles; the charger answers `NotSupported`
- Client certificates (security profile 3)
- ISO 15118 and Plug & Charge
- EVs as persistent entities
- Load-testing features: ramp-up profiles, throughput reports, multi-machine runs
- Deliberate protocol violations (malformed messages, missing or late responses, wrong ordering)
- A build step, a model store or a registry
- A background daemon or system service; automatic start after reboot
- An HTTP API
- Assertions, conditions or loops in scenarios
- Commands `events`, `wait`, `pause`, `attach`

## 3. Concepts

| Concept | Meaning |
|---|---|
| **Chargerfile** | A YAML file declaring one model |
| **Model** | A kind of charger: vendor, model name, firmware, connectors, power, default configuration, behavior settings |
| **Charger** | One running simulated instance, identified only by its **charge point ID** |
| **Connector** | A numbered outlet on a charger (1, 2, ...), as in OCPP 1.6 |
| **Site** | A named group of chargers declared in a site file and started together |
| **EV** | The simulated vehicle on a connector; four parameters that exist only while plugged in |
| **EV preset** | A named set of EV parameters in a site file |
| **Scenario** | A file of timed actions run against a running site |

Every charger belongs to a site. A charger started with `plugyard run` belongs to an implicit site named after its charge point ID.

## 4. Command reference

Rules that apply to every command:

- Required values are positional arguments; optional values are flags.
- A charger argument accepts a glob pattern (`'FAST-*'`, `'*'`) and then acts on every match.
- Charger commands see chargers from all running sites. `-s, --site <name>` narrows to one site.
- The same charge point ID may exist in two running sites. A command whose charger argument is ambiguous fails and asks for `-s`.
- `--format json` is available on `ps`, `site ls`, `inspect`, `stats` and `logs` (one JSON object per line for `logs`).
- Exit codes: `0` success, `1` runtime failure, `2` usage or validation error.

### Site commands

Site commands act on the `site.yaml` in the current directory unless `-f, --file <path>` or `-s, --site <name>` says otherwise.

| Command | Effect |
|---|---|
| `plugyard site up [-d] [--speed N]` | Create the site's chargers from the site file, reusing saved state, and start them. Runs in the foreground showing logs; `-d` detaches. |
| `plugyard site stop` | Power off all chargers. They stay listed in `ps -a`. State is kept. |
| `plugyard site start` | Power on the chargers of a stopped site. |
| `plugyard site down [--reset]` | Stop and remove the chargers from the listing. Saved state is kept unless `--reset` is given, which wipes it. |
| `plugyard site ls [-a]` | List running sites; `-a` includes stopped sites and sites that only have saved state. |
| `plugyard site validate` | Check the site file and the Chargerfiles it references without starting anything. |

### Charger commands

| Command | Effect |
|---|---|
| `plugyard ps [-a]` | List running chargers with site, connection state and connector status; `-a` includes stopped ones. |
| `plugyard logs <charger> [-f] [--events] [--since <duration>]` | Show events and OCPP messages. |
| `plugyard stats` | Live table of power, energy delivered and message counts per charger. |
| `plugyard inspect <charger>` | Full state: model, configuration keys, charging profiles, connectors, transaction. |
| `plugyard start <charger>` | Power on. |
| `plugyard stop <charger>` | Orderly shutdown. |
| `plugyard restart <charger>` | `stop` then `start`. |
| `plugyard kill <charger>` | Power cut. |
| `plugyard rm [-f] <charger>` | Wipe the charger's saved state. Refuses a running charger unless `-f`, which stops it first. The charger is recreated fresh on the next `site up` if the site file still declares it. |
| `plugyard run <Chargerfile> --csms <url> --id <charge point ID>` | Start one charger without a site file, in an implicit site named after the ID. |

`run` stays in the foreground printing the charger's log until interrupted, then shuts the charger down in an orderly way. It also accepts `--password <password>` for HTTP Basic Auth, which falls back to the `PLUGYARD_CSMS_PASSWORD` environment variable so the password can stay off the command line, and `--insecure-skip-verify`.

### Actions

```
plugyard exec <charger> <action> [arguments] [-n, --connector <number>]
```

| Action | Arguments | Effect |
|---|---|---|
| `plug` | `--ev <preset>`, `--battery`, `--soc`, `--max-power`, `--target-soc` | Plug an EV into the connector |
| `unplug` | | Remove the EV |
| `tap` | `<idTag>` | Present an RFID tag: `Authorize`, then start or stop the transaction |
| `fault` | `<errorCode>` | Report a fault with `StatusNotification` |
| `clear` | | Recover from the fault |
| `offline` | | Drop the network connection; the charger keeps running |
| `online` | | Restore the network connection |
| `reboot` | | Reboot the charger |

- `--connector` defaults to 1 for `plug`, `unplug` and `tap`.
- Giving `--connector` to a whole-charger action (`offline`, `online`, `reboot`) is an error.
- `plugyard exec <charger> --help` lists the actions.

### Other commands

| Command | Effect |
|---|---|
| `plugyard scenario run <file>` | Run a scenario against a running site. |
| `plugyard init` | Write a sample `site.yaml` and Chargerfile. |
| `plugyard version` | Print the version and supported OCPP versions. |
| `plugyard info` | Print running sites, charger counts, CSMS URLs and the data directory location. |

## 5. Chargerfile

YAML, in a file named `Chargerfile` or `*.chargerfile.yaml`. A JSON schema is published for editor support.

```yaml
vendor: Acme
model: FastOne 50
firmware: 1.4.2
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
  - { id: 2, type: CHAdeMO, maxPower: 50kW }
config:                      # OCPP configuration keys and their defaults
  HeartbeatInterval: 300
  MeterValueSampleInterval: 60
onEvFull: stop               # stop | suspend
startOn: tap                 # tap | plug
startTag: FREEVEND           # idTag used when startOn is plug
```

| Field | Meaning |
|---|---|
| `vendor`, `model`, `firmware` | Sent in `BootNotification` |
| `connectors` | Numbered outlets with type and maximum power |
| `config` | Default values of OCPP configuration keys |
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
evs:
  compact:   { battery: 40kWh, maxPower: 50kW }
  longrange: { battery: 82kWh, maxPower: 170kW, soc: 10% }
chargers:
  fast:
    model: ./fast.Chargerfile
    count: 20
    id: "FAST-{n:03}"                  # FAST-001 ... FAST-020
    ev: longrange                      # default EV for these chargers
  lobby:
    model: ./ac.Chargerfile
    id: "LOBBY-1"
    config: { MeterValueSampleInterval: 10 }
```

- `name` identifies the site and must be unique per user.
- `csms` applies to the whole site; a charger entry may override it.
- `${VAR}` is replaced from the environment, so secrets stay out of the file.
- Each entry under `chargers` instantiates a model `count` times (default 1) using the `id` pattern, with optional `config` overrides and a default `ev`.
- Entry names (`fast`, `lobby`) are used only inside the file. Everywhere else a charger is known by its charge point ID.

## 7. EV model

| Parameter | Flag | Built-in default |
|---|---|---|
| Battery capacity | `--battery` | 60 kWh |
| Starting state of charge | `--soc` | 20% |
| Maximum power accepted | `--max-power` | no limit; the connector's maximum applies |
| Target state of charge | `--target-soc` | 100% |

Precedence: flags on `plug`, then the preset named by `--ev`, then the charger entry's default `ev`, then the built-in defaults.

Charge curve: full available power up to 80% state of charge, then a linear taper to the target. Available power is the lowest of the connector maximum, the EV maximum and any active charging profile limit.

## 8. Charger behavior

### Lifecycle

| Trigger | Simulates | What the CSMS sees |
|---|---|---|
| `start` | Power on | `BootNotification`, then `StatusNotification` for each connector |
| `stop` | Orderly shutdown | Active transactions end with `StopTransaction`, then the connection closes |
| `kill`, or the machine shutting down | Power cut | The connection drops with no message. On the next start the charger boots and sends `StopTransaction` with reason `PowerLoss` and the last saved meter reading |
| `exec ... offline` | Network loss | The connection drops. Charging continues, and transaction messages are queued and replayed in order on `online` |

### Connection

- The charger asks for the `ocpp1.6` WebSocket subprotocol. If the CSMS does not select it, the charger logs a warning and carries on.
- If the CSMS answers `BootNotification` with `Pending` or `Rejected`, the charger sends nothing else and repeats `BootNotification` after the interval the CSMS gave.
- The heartbeat interval is the one in the `BootNotification` answer, or the `HeartbeatInterval` configuration key when the answer gives none.
- When the connection is lost or cannot be made, the charger keeps retrying, waiting 1 second at first and doubling up to 30 seconds, and boots again once connected.

### Transactions

- With `startOn: tap`, a transaction starts after plug-in and an accepted tag, in either order, and stops on a second tap of the same tag, on unplug, or on a remote stop.
- With `startOn: plug`, a transaction starts on plug-in using `startTag`.
- When the EV reaches its target, `onEvFull` decides between `StopTransaction` followed by `Finishing`, and `SuspendedEV` with the transaction left open.
- When a charging profile limits a connector to zero during a transaction, the connector reports `SuspendedEVSE` and returns to `Charging` when the limit rises.

### CSMS-initiated operations

Handled automatically with no user action: `RemoteStartTransaction`, `RemoteStopTransaction`, `Reset`, `ChangeConfiguration`, `GetConfiguration`, `ChangeAvailability`, `UnlockConnector`, `ClearCache`, `DataTransfer`, `TriggerMessage`, `SetChargingProfile`, `ClearChargingProfile`, `GetCompositeSchedule`.

### Time

`--speed N` (or `speed:` in the site file) multiplies the rate of EV charging and energy accumulation. Protocol timers such as heartbeat and meter value intervals run in real time, and message timestamps use the real clock.

## 9. Running and saved state

- There is no daemon. The `site up` process owns its chargers. Other commands find that process and talk to it over a local socket.
- A site does not start by itself after a reboot; run `plugyard site up -d` again.
- State is stored in one SQLite database per site, in a per-user data directory: `%LOCALAPPDATA%\plugyard` on Windows, `~/.local/share/plugyard` on Linux, `~/Library/Application Support/plugyard` on macOS.
- The database is accessed through a pure-Go SQLite driver so that all platforms build without a C compiler.

Saved per charger:

- A snapshot of the model and site-file settings it was created from
- Configuration keys and their current values
- Installed charging profiles
- Meter reading per connector
- Connector status and the open transaction, if any
- Messages queued while offline
- Authorization cache, if enabled

Write timing: configuration, charging profile and transaction changes are written immediately. Meter readings are written every few seconds and at each transaction event.

When files and saved state disagree:

- For anything the CSMS changed, saved state wins. `--reset` returns to the files.
- For structural edits (connectors added or removed, model changed, charger removed from the site file), `site up` lists the affected chargers and resets only those after confirmation.

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

- Each step is an `exec` action with a time offset from the start of the run.
- A scenario runs against a site that is already up.
- Exit code `0` when every step was delivered, non-zero otherwise.

## 11. Logs

- Two kinds of timestamped line per charger: events (connected, transaction started, fault) and raw OCPP messages in each direction.
- Each line is a timestamp, the charge point ID, a marker and the text. The marker is `|` for an event, `>` for a message sent to the CSMS and `<` for a message received from it.
- `logs` shows both; `--events` shows only events; `-f` follows; `--since 10m` limits the range.
- Logs are written to rotating, size-capped plain files beside the site's database. They are not stored in the database.

## 12. Distribution

- Prebuilt binaries for Windows, Linux and macOS, x86-64 and ARM64, attached to GitHub Releases and built automatically on each version tag
- `go install github.com/Amraa1/plugyard@latest`
- Homebrew, Scoop and winget later, on demand
