# plugyard

A terminal tool that simulates EV chargers, so you can develop and test a CSMS (charging station management system) without hardware. Its command line follows Docker's conventions: a file describes a kind of charger, a YAML file starts many of them together, and `ps` and `exec` operate on the running ones.

> **Beta.** plugyard does everything its [V1 specification](docs/SPEC.md) lists, and is tested against a fake CSMS and an independent OCPP library. It has not yet been used with many real CSMS products, so file formats and JSON output may still change before 1.0. Please [report](https://github.com/Amraa1/plugyard/issues) what does not work with yours.

## Install

**Windows**, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/Amraa1/plugyard/main/install.ps1 | iex
```

**Linux and macOS**:

```sh
curl -fsSL https://raw.githubusercontent.com/Amraa1/plugyard/main/install.sh | sh
```

Either one downloads the latest release for your system, checks it against the release's checksums, and puts `plugyard` in your user folder: `%LOCALAPPDATA%\Programs\plugyard` on Windows (added to your PATH), `~/.local/bin` elsewhere. No administrator rights are needed. Open a new terminal afterwards and run `plugyard version`, which prints the version you have.

Other ways:

- **With Go 1.26 or newer:** `go install github.com/Amraa1/plugyard@latest`
- **By hand:** download the archive for your system from the [releases page](https://github.com/Amraa1/plugyard/releases/latest), unpack it, and put `plugyard` somewhere on your PATH. Builds exist for Windows, Linux and macOS, each for x86-64 and ARM64.

The programs are not code-signed. If you download one by hand, Windows SmartScreen may say it is unrecognized (choose "More info", then "Run anyway"), and macOS may refuse to open it until you run `xattr -d com.apple.quarantine ./plugyard`. The install scripts and `go install` do not trigger these warnings.

### Update

```sh
plugyard update --check     # is there a newer release?
plugyard update             # install it
```

`plugyard update` replaces the program with the latest release, after checking the download against its checksums. Your saved chargers and logs are kept. Stop running sites first (`plugyard site stop`): a site started by one version cannot be controlled by another. If you installed with `go install`, run that command again instead.

### Uninstall

Delete the `plugyard` program, and the data folder that `plugyard info` shows.

## Quick start

You need a CSMS that accepts OCPP 1.6J over WebSocket. The examples assume it listens at `ws://localhost:9000/ocpp`.

```sh
plugyard init my-site --csms ws://localhost:9000/ocpp   # writes site.yaml, Chargerfile and scenario.yaml
cd my-site
plugyard site up                                        # three chargers connect; Ctrl+C stops them
```

In a second terminal:

```sh
plugyard ps                              # the three chargers and their connectors
plugyard scenario run scenario.yaml      # plugs in, charges, faults one charger and unplugs, by script
plugyard logs CP-1                       # every OCPP message CP-1 exchanged with your CSMS
```

That is a working setup. The rest of this page explains the files `init` wrote and what else you can do.

## The files, step by step: twenty chargers

**1. Describe a kind of charger** in a file called `Chargerfile`:

```yaml
vendor: Acme
model: FastOne 50
firmware: 1.4.2
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
```

**2. Describe the site** in `site.yaml`, next to it:

```yaml
name: demo
csms:
  url: ws://localhost:9000/ocpp
chargers:
  fast:
    model: ./Chargerfile
    count: 20
    id: "FAST-{n}"
```

`count: 20` makes twenty chargers, and `{n}` in `id` is each one's number, so they are called `FAST-1` to `FAST-20`. Each connects to the CSMS URL with its own ID appended, such as `ws://localhost:9000/ocpp/FAST-1`.

**3. Check it, then start it:**

```sh
plugyard site validate      # lists the chargers the file would create
plugyard site up            # starts them and shows their logs; Ctrl+C shuts them down
```

That is all. In a second terminal:

```sh
plugyard ps                              # every charger, its name, connection and connector statuses
plugyard exec FAST-1 plug                # plug an EV into FAST-1
plugyard exec FAST-1 tap 04A1B2C3        # present a tag: Authorize, then StartTransaction
plugyard exec 'FAST-*' plug              # act on every charger whose ID matches
plugyard exec FAST-1 unplug              # ends the transaction with StopTransaction
```

A ready-made version of this site, with more settings shown, is in [examples/site.yaml](examples/site.yaml).

### Running in the background

```sh
plugyard site up -d         # returns once the site is running
plugyard site ls            # the sites that are running
plugyard site stop          # shuts the site down
plugyard site start         # brings it back as it was
```

The site keeps running after you close the terminal, and ends when you sign out.

### See what is happening

```sh
plugyard logs FAST-1 -f                  # events and every OCPP message, as they happen
plugyard logs 'FAST-*' --events --since 10m
plugyard site logs -f                    # the whole site
plugyard stats                           # live power, energy and message counts
plugyard inspect FAST-1                  # everything about one charger
plugyard info                            # every site on this machine, where data is kept, and which build this is
```

- **Logs** are kept on disk, so they are there for a site in the background and after a stop. A log holds the newest 30 MB; `log: { maxSize: 100MB, files: 5 }` in the site file changes that.
- **`inspect`** shows the configuration keys with their current values, the charging profiles your CSMS installed, the EV and transaction on each connector, and the messages still queued for the CSMS.
- **`stats`** refreshes every second in a terminal. `--no-stream` prints once.

#### In scripts and CI

Every listing command takes `--format json` and prints one object per line. Exit codes are `0` for success, `1` when something could not be done, and `2` for a mistake in the command or a file.

```sh
# Did FAST-1 send a StopTransaction?
plugyard logs FAST-1 --format json | jq -e 'select(.kind == "sent" and .message[2] == "StopTransaction")'

# Total power of the site, in watts
plugyard stats --format json | jq -s 'map(.powerW) | add'

# The value of one configuration key
plugyard inspect FAST-1 --format json | jq -r '.configuration[] | select(.key == "HeartbeatInterval").value'
```

### Run a scenario

A scenario is a file of timed actions, for tests that run by themselves:

```yaml
name: commute
steps:
  - { at: 0s,  charger: "FAST-*", do: plug, ev: longrange }
  - { at: 5s,  charger: "FAST-*", do: tap, tag: 04A1B2C3 }
  - { at: 10m, charger: FAST-3, do: fault, code: GroundFailure }
  - { at: 20m, charger: FAST-5, do: kill }      # a power cut mid-charge
  - { at: 21m, charger: FAST-5, do: start }
  - { at: 30m, charger: "FAST-*", do: unplug }
```

```sh
plugyard scenario validate commute.yaml      # check it, and see which chargers each step would act on
plugyard scenario run commute.yaml           # run it against the running site
plugyard scenario run commute.yaml --step 3  # try one step now, while you are writing it
```

- **A step does what `exec` does**, with the same options, or powers chargers with `stop`, `start`, `restart` and `kill`.
- **Times follow the site's speed.** Against `plugyard site up --speed 60`, these thirty minutes take thirty seconds, and the EVs have charged for thirty minutes.
- **It fits CI.** The exit code is `0` when everything succeeded, `1` when an action failed, and `2` when the file has a mistake. `--fail-fast` stops at the first failure, and `--format json` prints one object per action.

A longer one, with a network drop and a power cut, is in [examples/scenario.yaml](examples/scenario.yaml).

### Editor support

The files `plugyard init` writes start with a line that names their JSON schema, so an editor with YAML support (VS Code with the YAML extension, JetBrains IDEs) completes settings and marks mistakes as you type. For your own files, copy that first line, or print a schema with `plugyard schema site`, `plugyard schema chargerfile` or `plugyard schema scenario`.

### Chargers remember

A charger keeps its state when it is stopped: its meter, its configuration, the EV plugged into it, and anything it still owes the CSMS. So your CSMS meets the same chargers every time, with meters that never run backwards.

```sh
plugyard stop FAST-1          # orderly shutdown: StopTransaction, then the connection closes
plugyard start FAST-1         # the same charger boots again
plugyard kill FAST-1          # power cut: the connection drops with no message
plugyard start FAST-1         # it boots and reports the transaction with reason PowerLoss
plugyard ps -a                # also lists the stopped chargers
plugyard rm FAST-1            # wipes the charger's saved state
```

`kill` is how you test what your CSMS does when a charger loses power mid-transaction. The same happens if the machine crashes or plugyard is ended from the task manager: the next start reports `PowerLoss` with the last meter reading saved, which is at most five seconds old.

| To | Use |
|---|---|
| Stop a site and come back to it later | `plugyard site stop`, then `plugyard site start` |
| Apply changes made to the site file | `plugyard site up` |
| Take a site off the list but keep its state | `plugyard site down` |
| Start over with fresh chargers | `plugyard site down --reset` |

If you edit a site file so that saved state would be lost, for example by removing a charger, `site up` lists what is affected and asks first. `--yes` answers in advance, for scripts.

### Test what goes wrong

Real chargers fail, and a CSMS has to cope. plugyard lets you cause the failures you would otherwise wait months to see in the field, one command each.

**A hardware fault.** The connector reports `Faulted` with the error code, and a transaction running there ends:

```sh
plugyard exec FAST-1 fault GroundFailure
plugyard exec FAST-1 fault OverCurrentFailure --info "phase L2" --vendor-code E-42
plugyard exec FAST-1 clear               # the fault is gone
plugyard exec FAST-1 fault InternalError -n 0    # the whole charger, not one connector
```

The codes are the fifteen of OCPP 1.6: `ConnectorLockFailure`, `EVCommunicationError`, `GroundFailure`, `HighTemperature`, `InternalError`, `LocalListConflict`, `OtherError`, `OverCurrentFailure`, `OverVoltage`, `PowerMeterFailure`, `PowerSwitchFailure`, `ReaderFailure`, `ResetFailure`, `UnderVoltage` and `WeakSignal`. `plugyard exec --help` lists them too.

**A lost connection.** The charger keeps charging, queues what it has to say, and sends it in order when it is back:

```sh
plugyard exec FAST-1 offline             # stays away until...
plugyard exec FAST-1 online
plugyard exec 'FAST-*' offline --for 30s # twenty chargers drop out and return
```

**A reboot, or a power cut:**

```sh
plugyard exec FAST-1 reboot              # ends the transaction, boots again
plugyard kill FAST-1                     # no goodbye; PowerLoss on the next start
```

| What happens in the field | How to cause it | What your CSMS sees |
|---|---|---|
| A connector faults mid-charge | `exec ... fault <code>` | `StopTransaction` (reason `Other`), then `StatusNotification` `Faulted` with the code |
| The charger's network drops | `exec ... offline` | Silence, then `BootNotification` and the queued messages in order |
| Your CSMS restarts | Restart your CSMS | The charger retries by itself, boots and replays |
| Your CSMS answers a message with an error | Return a `CallError` | The same message again, up to three times |
| The charger reboots | `exec ... reboot`, or send `Reset` | `StopTransaction` (reason `Reboot`, `SoftReset` or `HardReset`), then `BootNotification` |
| The charger loses power | `plugyard kill` | The connection drops; `StopTransaction` (reason `PowerLoss`) after the next start |

### Your CSMS is in charge

The charger answers the requests a CSMS sends, with no action from you: `RemoteStartTransaction`, `RemoteStopTransaction`, `Reset`, `GetConfiguration`, `ChangeConfiguration`, `ChangeAvailability`, `UnlockConnector`, `ClearCache`, `DataTransfer`, `TriggerMessage`, and the smart charging requests below. A configuration key your CSMS changes is saved, and stays changed across restarts. What each request does is in [docs/SPEC.md](docs/SPEC.md), section 8.

### Smart charging

Send your CSMS's charging profiles and watch the charger obey them:

| Your CSMS sends | The charger |
|---|---|
| `SetChargingProfile` with a 16 A limit | Delivers 11.04 kW from then on (16 A x 230 V x 3 phases) |
| A limit of 0 | Reports `SuspendedEVSE`, keeps the transaction open, and returns to `Charging` when the limit rises |
| `ClearChargingProfile` | Goes back to full power |
| `GetCompositeSchedule` | Answers with the limit that results from every installed profile |

- All three purposes (`TxProfile`, `TxDefaultProfile`, `ChargePointMaxProfile`), stack levels, and `Absolute`, `Relative` and `Recurring` schedules work as OCPP 1.6 describes.
- Limits in W or in A are both accepted. For amps, tell the Chargerfile what the supply is if it is not 3 phases of 230 V: `phases: 1` for a single-phase wallbox.
- To see the limit from your CSMS, add `Power.Offered` or `Current.Offered` to the `MeterValuesSampledData` key. The charger's log also says when a limit changes.
- Schedules follow the real clock even with `--speed`, so for a quick test send periods a few seconds long.

Profiles are saved with the charger. The rules are in [docs/SPEC.md](docs/SPEC.md), section 8.

### Names

Every charger also gets a name, as Docker containers do: `brisk_volt`, `quiet_ampere`. A name is unique on your machine, so it always means one charger, even when two sites use the same charge point ID. Use it wherever a command takes a charger:

```sh
plugyard exec brisk_volt plug
```

Give a charger a name of your own with `name:` in the site file (on an entry that creates one charger) or `--name` on `plugyard run`. Your CSMS never sees the name, only the charge point ID.

### When you need more from the IDs

| In the site file | Charge point IDs |
|---|---|
| `id: "FAST-{n}"`, `count: 3` | `FAST-1`, `FAST-2`, `FAST-3` |
| `id: "FAST-{n:03}"`, `count: 3` | `FAST-001`, `FAST-002`, `FAST-003` |
| `id: "{n:012}"`, `count: 2`, `start: 202310090165` | `202310090165`, `202310090166` |
| `ids: [12994897, p025b2036387]` | exactly those two |

### Other things a site file can do

- **Keep secrets out of the file.** `password: ${CSMS_PASSWORD}` reads an environment variable, and `${CSMS_URL:-ws://localhost:9000/ocpp}` falls back to a default when the variable is not set.
- **Describe EVs once.** Name a set of EV parameters under `evs:` and plug it in with `plugyard exec FAST-1 plug --ev longrange`.
- **Charge like a fast-charging car.** `curve: dc-fast` in an EV preset, or `plug --curve dc-fast`, makes power ramp up, hold, and taper off as the battery fills.
- **Speed time up.** `plugyard site up --speed 60` makes one real second count as a minute of charging.
- **Go easy on the CSMS.** `--connect-rate 50` opens at most fifty new connections a second; by default every charger connects at once.

All of it is specified in [docs/SPEC.md](docs/SPEC.md), sections 5 to 7.

## One charger, no site file

```sh
plugyard run Chargerfile --csms ws://localhost:9000/ocpp --id CP-1
```

It runs in the foreground until you press Ctrl+C. Its state is saved too: run the same `--id` again and the same charger comes back. Add `--rm` for a throwaway charger that saves nothing, and `--password` (or set `PLUGYARD_CSMS_PASSWORD`) for Basic Auth over `wss://`.

## How many chargers

One site of 2,000 chargers runs comfortably on a laptop. In the project's own test, 2,000 chargers connected in about one second, each ran a transaction with meter values, and all shut down cleanly, using under 400 MB for the site and the test CSMS together. The figures are in [docs/SPEC.md](docs/SPEC.md), section 9.

Your CSMS may want them to arrive more gently: `plugyard site up --connect-rate 50` opens at most fifty connections a second.

## What plugyard does not do

- OCPP 2.0.1; only OCPP 1.6J
- Firmware updates, local authorization lists and reservations; the charger answers these `NotSupported`
- Deliberately broken messages, to test how a CSMS handles protocol violations
- Client certificates (security profile 3)

## Building from source

Requires Go 1.26 or newer.

```sh
go build .
./plugyard version
go test ./...
```

`PLUGYARD_SCALE=2000 go test -run TestScale -v ./internal/cli` runs the 2,000-charger test. A release is made by pushing a version tag such as `v0.2.0`; [GoReleaser](https://goreleaser.com) then builds and publishes the six archives.

## Documentation

- [docs/SPEC.md](docs/SPEC.md): the V1 specification
- [CONTEXT.md](CONTEXT.md): glossary of the project's terms
- [docs/adr/](docs/adr/): records of the main design decisions

## License

[Apache-2.0](LICENSE)
