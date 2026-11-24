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
| **Charger** | One simulated instance, known to the CSMS only by its **charge point ID** |
| **Name** | A second handle for a charger, used only on the command line and unique on the machine. It is given by the user or made up, as Docker names containers |
| **Connector** | A numbered outlet on a charger (1, 2, ...), as in OCPP 1.6 |
| **Site** | A named group of chargers declared in a site file and started together |
| **EV** | The simulated vehicle on a connector; four parameters that exist only while plugged in |
| **EV preset** | A named set of EV parameters in a site file |
| **Charge curve** | The share of its maximum power an EV accepts at each state of charge |
| **Scenario** | A file of timed actions run against a running site |

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
| `plugyard site up [-d] [-y] [--speed N] [--connect-rate N]` | Read the site file, create the site's chargers, reusing saved state, and start all of them. Runs in the foreground showing the log of every charger, as `docker compose up` does; `-d` runs it in the background. |
| `plugyard site stop` | Power off all chargers. They stay listed in `ps -a`. State is kept. |
| `plugyard site start` | Power on every charger of a stopped site, in the background, exactly as saved. No file is read. |
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
| `plugyard ps [-a]` | List running chargers with name, site, connection state (`connecting`, `online` or `offline`) and the status of each connector; `-a` includes stopped ones, shown as `stopped` with the status each connector will report on start. |
| `plugyard logs <charger> [-f] [--events] [--since <when>] [--tail N]` | Show events and OCPP messages (section 11). |
| `plugyard stats [--no-stream]` | Live table of power, energy on the meters, transactions and message counts per running charger, with a total first. |
| `plugyard inspect <charger>` | Full state of one charger: model, connectors, EVs, transactions, charging profiles, configuration keys and the messages still owed to the CSMS. Works on a stopped charger. Never shows a password. |
| `plugyard start <charger>` | Power on. |
| `plugyard stop <charger>` | Orderly shutdown. State is kept. |
| `plugyard restart <charger>` | `stop` then `start`. |
| `plugyard kill <charger>` | Power cut. State is kept as it was last saved. |
| `plugyard rm [-f] <charger>` | Wipe the charger's saved state. Refuses a running charger unless `-f`, which stops it first. The charger is recreated fresh on the next `site up` if the site file still declares it. |
| `plugyard run <Chargerfile> --csms <url> --id <charge point ID>` | Start one charger without a site file, in an implicit site named after the ID. |

`start`, `stop`, `restart`, `kill` and `rm` follow Docker, and print one line per charger as `exec` does:

- `stop` asks, `kill` cuts, and only `rm` deletes. A killed charger still exists and comes back with `start`.
- Stopping, killing or removing the last running charger of a site ends the site's process; the site is then `stopped`.
- `start` on a charger whose site has no process starts the site in the background with only the named chargers powered on. The others stay stopped until they are started or `site start` is run.
- `stop` on a charger that is already stopped succeeds. `kill` on one fails.
- `rm` does not ask for confirmation. Removing the last charger of a site removes the site.

`run` stays in the foreground printing the charger's log until interrupted, then shuts the charger down in an orderly way. Its state is saved like that of any other charger: running the same charge point ID again brings the same charger back, and `plugyard rm <ID>` wipes it. `--name <name>` gives the charger its name. `--rm` saves nothing, for a throwaway charger. `-y` answers the question `site up` asks when the Chargerfile no longer fits the saved state. It also accepts `--speed N`, with the same meaning as on `site up`, `--password <password>` for HTTP Basic Auth, which falls back to the `PLUGYARD_CSMS_PASSWORD` environment variable so the password can stay off the command line, and `--insecure-skip-verify`.

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

- `--connector` defaults to 1 for `plug`, `unplug`, `tap`, `fault` and `clear`.
- `fault` and `clear` also accept `--connector 0`, which means the charger as a whole, as in OCPP.
- Giving `--connector` to a whole-charger action (`offline`, `online`, `reboot`) is an error.
- `plugyard exec --help` lists the actions and the error codes.
- `exec` waits for the immediate outcome of the action and prints it, for example that a tag was accepted and a transaction started, or that the CSMS rejected the tag.
- The exit code is `0` when the action was delivered, whatever the CSMS answered. A rejected tag is the CSMS's answer and not a failure. Exit code `1` means the action could not be carried out, for example because no charger matched or the connector already has an EV.
- `exec` prints one line per matched charger, sorted by charge point ID: the ID, the connector, `ok` or `failed`, and the outcome or the reason. When the charger argument matched several chargers the action runs on every one of them, a summary line follows (`18 of 20 succeeded, 2 failed`), and the exit code is `1` if any failed. Chargers on which the action worked are not rolled back.
- `exec` also accepts `--format json`.

### Other commands

| Command | Effect |
|---|---|
| `plugyard scenario run <file> [-s <site>] [--fail-fast] [--step N] [--from N] [--to N] [--speed N]` | Run a scenario against a running site (section 10). |
| `plugyard scenario validate <file> [-s <site>]` | Check a scenario and show its plan without running it. |
| `plugyard init [directory] [--csms <url>] [--force]` | Write a starter `site.yaml`, `Chargerfile` and `scenario.yaml`. |
| `plugyard schema <chargerfile\|site\|scenario>` | Print the JSON schema of a file type. |
| `plugyard version` | Print the version and supported OCPP versions. |
| `plugyard info` | Print the version, the data directory, and every site with its status, charger counts and CSMS URLs. |
| `plugyard update [--check]` | Replace the program with the latest release (section 13). |

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
- The EV stays plugged in across a stop or a power cut, with its state of charge. After the start the connector reports `Finishing` until the EV is unplugged. No transaction starts by itself, even with `startOn: plug`.
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
- A tap on a connector that is `Finishing` is refused: the EV must be unplugged before a new transaction can start.
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
| `RemoteStartTransaction` | Behaves as after an accepted tap: starts a transaction if an EV is plugged in, and otherwise reports `Preparing` and waits `ConnectionTimeOut` seconds for one. With no connector named it picks one with an EV waiting, then the lowest-numbered free one. `Rejected` when no connector is free. With `AuthorizeRemoteTxRequests` set to `true` (default `false`) it sends `Authorize` for the tag first |
| `RemoteStopTransaction` | Stops the transaction with reason `Remote`. `Rejected` for a transaction ID it does not have |
| `Reset` | Ends transactions with `StopTransaction`, reason `SoftReset` or `HardReset`, then reboots: it is away for about a second and boots again. A soft reset closes the connection properly; a hard reset drops it |
| `GetConfiguration` | Reports the keys asked for, or all of them, each with its value and whether it is read-only |
| `ChangeConfiguration` | Changes a key at once and saves it. `Rejected` for a read-only key or a value that does not fit, such as text for a number. `NotSupported` for a key the charger does not have |
| `ChangeAvailability` | Makes a connector `Unavailable` or available again. A connector in a transaction answers `Scheduled` and becomes `Unavailable` when the transaction ends. Connector 0 means the charger and all its connectors. Availability is saved |
| `UnlockConnector` | Ends a transaction on the connector with reason `UnlockCommand` and answers `Unlocked`. The EV stays until it is unplugged |
| `ClearCache` | Empties the authorization cache |
| `DataTransfer` | `Accepted` for the vendor IDs the Chargerfile lists under `dataTransfer`, `UnknownVendorId` otherwise |
| `TriggerMessage` | Sends the `BootNotification`, `Heartbeat`, `MeterValues` or `StatusNotification` asked for. `NotImplemented` for the firmware and diagnostics messages |

- Requests of Firmware Management, Local Auth List Management and Reservation are answered with the error `NotSupported`.
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

### Failures

Failures are caused on purpose, with actions, so that a CSMS can be tested against them.

**Faults.** `exec <charger> fault <errorCode>` makes a connector report `Faulted` with one of the OCPP 1.6 error codes: `ConnectorLockFailure`, `EVCommunicationError`, `GroundFailure`, `HighTemperature`, `InternalError`, `LocalListConflict`, `OtherError`, `OverCurrentFailure`, `OverVoltage`, `PowerMeterFailure`, `PowerSwitchFailure`, `ReaderFailure`, `ResetFailure`, `UnderVoltage`, `WeakSignal`.

- A transaction on the connector ends with reason `Other`.
- While faulted, an EV can still be plugged in or unplugged, but a tap and a remote start are refused.
- `clear` returns the connector to `Available`, or to `Finishing` if an EV is still plugged in.
- With `--connector 0` the fault is the charger's: it is reported for connector 0, every transaction ends, and nothing starts on any connector until it is cleared.
- A fault is saved, and is reported again when the charger next boots.

**Network loss.** The charger cannot tell why the CSMS is out of reach, so it behaves the same whatever the cause: charging continues, transaction messages queue in order, and after reconnecting it boots and sends them before anything new.

- `exec <charger> offline` takes the connection down until `online`. `--for 30s` brings it back after that time.
- When the CSMS goes away by itself, the charger retries by itself, as described under Connection.
- A tap while offline is refused, since the tag cannot be authorized, unless one of two standard keys allows it. `AllowOfflineTxForUnknownId: true` accepts any tag. `AuthorizationCacheEnabled: true` has the charger remember what the CSMS said about each tag and accept the ones it accepted; the cache is saved, and `ClearCache` empties it. Both are `false` by default.
- Stopping a transaction while offline always works.
- `offline` is not remembered across `stop` and `start`.

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
- A snapshot of the model and site-file settings it was created from
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
| Vendor, model name or firmware version | Applied. The next `BootNotification` carries the new values, as after a firmware update |
| Connector type or maximum power, behavior settings, CSMS URL, default EV | Applied |
| A configuration key | The file wins, unless the CSMS changed that key; then saved state wins |
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
| `offline` | `for` | `exec <charger> offline` |
| `online`, `reboot` | | `exec <charger> online`, `reboot` |
| `stop`, `start`, `restart`, `kill` | | `plugyard stop <charger>` and so on |

- Steps are written in the order they run. A step whose `at` is earlier than the step above it is an error. Steps with the same `at` run one after the other, in file order.
- Unknown fields, and an option on an action it does not apply to, are errors. Every problem in the file is reported at once.
- There are no assertions, conditions or loops.

### Running a scenario

- A scenario runs against a site that is already up: the one named with `-s`, or the only one running. With several running and no `-s`, the command stops and says so. Patterns match only chargers of that site.
- Step times follow the site's speed. Against a site at `--speed 60`, a step at `30m` runs after 30 seconds, when the EVs have charged for 30 simulated minutes. `--speed N` on `scenario run` overrides the site's speed.
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

## 12. Getting started and editor support

`plugyard init` writes three files into the current directory, or the one named: `site.yaml` with three chargers, the `Chargerfile` they are made from, and `scenario.yaml`, a sample scenario.

- `plugyard site up` works on them as they are. The chargers connect to the `CSMS_URL` environment variable, or to `ws://localhost:9000/ocpp` when it is not set. `--csms <url>` writes an address into the site file instead.
- A file that exists is never overwritten: `init` stops and writes nothing. `--force` overwrites.
- It ends by printing the next three commands to run.

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

- `PLUGYARD_INSTALL_DIR` names another folder for the install scripts, and `PLUGYARD_NO_MODIFY_PATH` keeps `install.ps1` from changing the PATH.
- Homebrew, Scoop and winget later, on demand.

`plugyard update`:

- Finds the latest release, and stops if the installed version is that one or newer. `--check` only reports.
- Refuses while any site is running, and says which: a site started by one version cannot be controlled by another, since the channel between them may change.
- Downloads the archive for the system it runs on, checks it against the release's `checksums.txt`, and replaces the program. If anything fails, the program that was there stays.
- On Windows a running program cannot be deleted, so the old one is renamed and removed the next time plugyard starts.
- Saved state and logs are kept.
- `PLUGYARD_RELEASES_URL` points the install scripts and `plugyard update` at another copy of the releases, such as a mirror inside a company network.
- The checksum guards against a damaged download. It does not prove who published the release.
