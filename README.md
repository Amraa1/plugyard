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
- **With Docker:** `docker pull amraa1/plugyard`, see [In a container](#in-a-container)
- **By hand:** download the archive for your system from the [releases page](https://github.com/Amraa1/plugyard/releases/latest), unpack it, and put `plugyard` somewhere on your PATH. Builds exist for Windows, Linux and macOS, each for x86-64 and ARM64.

The programs are not code-signed. If you download one by hand, Windows SmartScreen may say it is unrecognized (choose "More info", then "Run anyway"), and macOS may refuse to open it until you run `xattr -d com.apple.quarantine ./plugyard`. The install scripts and `go install` do not trigger these warnings.

### Update

```sh
plugyard update --check     # is there a newer release?
plugyard update             # install it
```

`plugyard update` replaces the program with the latest release, after checking the download against its checksums. Your saved chargers and logs are kept. If you installed with `go install`, run that command again instead.

Sites and the API that are running are restarted on the new version, because a site started by one version cannot be controlled by another. `plugyard update` says which ones and asks first; `--yes` answers in advance. Transactions that are running end with `StopTransaction`, and the chargers boot again, as real chargers would after a restart.

### Uninstall

```sh
plugyard uninstall
```

It shows what it is about to remove, with the full paths, and asks first:

- the program, and its folder when that folder is plugyard's own;
- on Windows, the entry the install script added to your PATH;
- your saved chargers and logs. `--keep-data` leaves these, for when you plan to install again.

Sites and the API that are running are stopped first, and the question names them. `--yes` skips the question, for scripts. Files that `plugyard init` wrote in your own folders are yours and stay.

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
- **Prefer `docker container ls` to `docker ps`?** Every charger command is also in the `charger` suite: `plugyard charger ls` is `plugyard ps`, `plugyard charger logs` is `plugyard logs`, and so on.

#### In scripts and CI

Every listing command takes `--format json` and prints one object per line. Exit codes are `0` for success, `1` when something could not be done, and `2` for a mistake in the command or a file.

```sh
# Wait until FAST-1 is charging, in place of a guessed sleep; exit code 1 if it is not within 30 seconds
plugyard exec FAST-1 tap 04A1B2C3
plugyard wait FAST-1 Charging

# The same for several chargers, a connection state, and a longer time
plugyard wait 'FAST-*' online --timeout 2m

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
- **Every time follows the site's speed.** Against `plugyard site up --speed 60`, these thirty minutes take thirty seconds, and the EVs have charged for thirty minutes. The `for` of an `offline` step is shortened the same way, where `exec ... offline --for` is real time.
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

### Keep a site in a known state

For a server that other people look at, such as a staging site that must always show a charger of every kind, a site file can declare the state each charger is in whenever it powers on:

```yaml
chargers:
  lobby:
    model: ./wallbox.Chargerfile
    id: LOBBY-1
    state: { connectors: { 1: { ev: longrange } } }          # an EV is plugged in: Preparing
  bay:
    model: ./Chargerfile
    id: BAY-1
    state: { connectors: { 1: { fault: GroundFailure } } }   # Faulted
  free:
    model: ./Chargerfile
    id: FREE-1
    state: { connectors: { 1: {} } }                         # nothing plugged in: Available
  gone:
    model: ./Chargerfile
    id: GONE-1
    state: { connection: offline }                           # stays away from the CSMS
  spare:
    model: ./Chargerfile
    id: SPARE-1
    state: { connection: stopped }                           # declared, and never started
```

Declared state wins over what was saved, every time the charger powers on, so a restart of the server cannot drift from the file. What you change by hand, with `exec` or a scenario, lasts until then, and `plugyard restart BAY-1` puts one charger back. A charger declared `stopped` starts with `plugyard start SPARE-1`. `plugyard site validate` and `plugyard inspect` show what is declared.

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

The charger answers the requests a CSMS sends, with no action from you: `RemoteStartTransaction`, `RemoteStopTransaction`, `Reset`, `GetConfiguration`, `ChangeConfiguration`, `ChangeAvailability`, `UnlockConnector`, `ClearCache`, `DataTransfer`, `TriggerMessage`, and the smart charging and firmware requests below. A configuration key your CSMS changes is saved, and stays changed across restarts. What each request does is in [docs/SPEC.md](docs/SPEC.md), section 8.

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

### Firmware updates and diagnostics

Press "update firmware" in your CSMS and watch the charger go through it. No firmware file is needed: the charger fetches nothing, it reports what a real charger reports. Diagnostics are real: the charger uploads its own log to the place your CSMS names.

| Your CSMS sends | The charger |
|---|---|
| `UpdateFirmware` for `ftp://host/fw-2.4.1.bin` | Reports `Downloading`, `Downloaded`, `Installing` and `Installed`, then reboots and sends `BootNotification` with firmware `fw-2.4.1` |
| `UpdateFirmware` during a transaction | Downloads, then waits for the transaction to end before it installs |
| `GetDiagnostics` for `ftp://user:password@host/dir/` | Answers with a file name, reports `Uploading`, uploads its log there under that name, and reports `Uploaded`, or `UploadFailed` if the server cannot be reached or refuses |

To test the unhappy path, tell the charger what goes wrong next:

```sh
plugyard exec FAST-1 fail download       # the next update ends with DownloadFailed
plugyard exec FAST-1 fail install        # ... or with InstallationFailed
plugyard exec FAST-1 fail upload         # the next diagnostics upload ends with UploadFailed
```

- Each step of a firmware update takes 10 seconds, or less with `--speed`.
- The diagnostics file holds what `plugyard logs FAST-1` shows, and `startTime` and `stopTime` choose the lines. It is sent the way the location says:

  | Location | Sent with |
  |---|---|
  | `ftp://`, `ftps://` | FTP, or FTP with encryption |
  | `sftp://` | SFTP, over SSH |
  | `http://`, `https://` | An HTTP `PUT` |

  For an `ftps://` or `https://` server with a self-signed certificate, add `insecureSkipVerify: true` under `uploads` in the site file.
- **A charger uploads only to the machine its CSMS is on**, unless you allow more. Your CSMS chooses the address, so this keeps a CSMS that has been broken into from pointing the upload at your internal network. A CSMS and a file server on your own machine work with no setting. For a file server elsewhere, name it in the site file:

  ```yaml
  uploads: { allow: [files.example.com] }
  ```

  With `plugyard run`, use `--allow-upload files.example.com`. A refused upload ends in `UploadFailed`, and the charger's log shows the line to add.

The details are in [docs/SPEC.md](docs/SPEC.md), section 8.

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

- **Say what state each charger is in.** A `state:` block on a charger entry declares it offline, stopped, faulted or with an EV plugged in, each time it powers on. See "Keep a site in a known state" above.

All of it is specified in [docs/SPEC.md](docs/SPEC.md), sections 5 to 7.

## One charger, no site file

```sh
plugyard run Chargerfile --csms ws://localhost:9000/ocpp --id CP-1
```

It runs in the foreground until you press Ctrl+C. Add `-d` to run it in the background; `plugyard stop CP-1` then shuts it down. Its state is saved too: run the same `--id` again and the same charger comes back. Add `--rm` for a throwaway charger that saves nothing, and `--password` (or set `PLUGYARD_CSMS_PASSWORD`) for Basic Auth over `wss://`.

## With an AI agent

Coding agents such as Claude Code and Codex can drive plugyard for you: start chargers, plug in, tap, and check what your CSMS answered.

**Give the agent the skill.** In your project folder:

```sh
plugyard init --agents
```

This installs the [plugyard skill](skills/plugyard/SKILL.md) with the [skills tool](https://skills.sh), so an agent that opens a session there knows plugyard without being told. In an empty folder it also writes the starter files; in a folder that has them it only adds the skill.

- It runs `npx skills add Amraa1/plugyard`, which you can also run yourself. That needs [Node.js](https://nodejs.org).
- The skills tool asks which agents you use, and puts the skill where each of them looks.
- The skills tool counts installs anonymously. Set `DISABLE_TELEMETRY=1` to turn that off.
- No Node.js? Copy [`skills/plugyard`](skills/plugyard) into `.claude/skills/` (Claude Code) or `.agents/skills/` of your project.

**Or just tell it.** An agent that has no skill needs one sentence: "use plugyard; run `plugyard agents` first". That command prints a guide written for agents, matching the version you have installed. The same guide is [`llms.txt`](llms.txt).

**Is plugyard on a server, and the agent on your machine?** Then the agent uses plugyard's tools over the network: see [Tools for an AI agent](#tools-for-an-ai-agent-mcp) below.

## From another machine: the API

On a shared server, the people who need a charger to do something often have no shell there: the app team wants to plug an EV in, the dashboard team wants to see a fault. The API gives them the actions of `plugyard exec` over HTTP.

```sh
plugyard api token create mobile-team --site staging   # prints the token, once
plugyard api up -d --listen 0.0.0.0:7070               # serve it to other machines
```

From the other machine:

```sh
curl -H "Authorization: Bearer $TOKEN" https://plugyard.example.com/v1/chargers
curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"tag": "04A1B2C3"}' https://plugyard.example.com/v1/chargers/FAST-1/tap
```

| To | Call |
|---|---|
| List every charger | `GET /v1/chargers` |
| See everything about one | `GET /v1/chargers/FAST-1` |
| Plug in, unplug, tap, fault, clear, go offline or online, reboot, fail | `POST /v1/chargers/FAST-1/<action>`, with the options as JSON |
| Wait until a charger is in a state, in place of a guessed pause | `GET /v1/chargers/FAST-1/wait?state=Charging` |
| Read the events and OCPP messages of a charger | `GET /v1/chargers/FAST-1/logs?since=10m` |

- **It listens on this machine only** unless you say otherwise, and it refuses to listen anywhere else until a token exists.
- **Each token has a name**, can be limited to one site with `--site`, and can be `--read-only`. `plugyard api logs --charger FAST-1` shows who did what to a charger.
- **It only acts on chargers and reads them.** Starting sites, updating plugyard and anything that touches files stay on the command line.
- **Put HTTPS in front** when it leaves the machine: a reverse proxy, or `--tls-cert` and `--tls-key`.
- `plugyard api spec` prints the full description as an OpenAPI document, and `plugyard api down` stops the API.

The details are in [docs/SPEC.md](docs/SPEC.md), section 14.

### Tools for an AI agent (MCP)

The API also serves its reading and actions as tools for AI agents, over the Model Context Protocol, at `/mcp`. An agent on your machine can then plug in, tap, cause a fault, wait for a state and read the log of chargers that run on a server. In Claude Code:

```sh
claude mcp add --transport http plugyard https://plugyard.example.com/mcp \
       --header "Authorization: Bearer $TOKEN"
```

Then ask for what you want: "plug an EV into FAST-1, start a session with tag 04A1B2C3, and tell me what the CSMS answered".

- **Nothing more to start**: `plugyard api up` serves the tools, with the same tokens and the same rules. `plugyard api status` shows the address.
- **Give the agent a token of its own**, limited to a site if it should stay there. `plugyard api logs` then shows what the agent did.
- **A `--read-only` token gets the tools that read** (`list_chargers`, `inspect_charger`, `wait_for_state`, `read_logs`) and none that act.
- **The tools that act** are `plug`, `unplug`, `tap`, `fault`, `clear_fault`, `go_offline`, `go_online`, `reboot` and `fail_next`. They cannot start or remove chargers.

## In a container

plugyard is on Docker Hub as [`amraa1/plugyard`](https://hub.docker.com/r/amraa1/plugyard). To try your CSMS against three chargers, with nothing to install and no files:

```sh
docker run --rm -e CSMS_URL=ws://host.docker.internal:9000/ocpp amraa1/plugyard
```

On a server, where the chargers are to stay and others are to reach the API, use [examples/docker/compose.yaml](examples/docker/compose.yaml). Copy it beside your `site.yaml`, then:

```sh
docker compose run --rm plugyard api token create mobile-team   # prints the token, once
docker compose up -d
docker compose exec plugyard plugyard ps
```

| In the container | Holds | Give it |
|---|---|---|
| `/site` | `site.yaml` and its Chargerfiles | Your folder, mounted there |
| `/data` | Saved chargers, logs and API tokens | A volume, or the chargers start from zero on every restart |
| Port `7070` | The API | A published port, behind HTTPS |

- **Everything after the image's name goes to `plugyard`**, so `amraa1/plugyard site up --speed 60 --api 0.0.0.0:7070` works as it would in a terminal. `--api` serves the API for as long as the site runs.
- **A CSMS on the same server is at `host.docker.internal`**, not at `localhost`, which inside a container is the container itself.
- **The image has no shell and does not run as root.** `docker compose exec plugyard plugyard <command>` runs any plugyard command in it.
- **It comes back by itself** after a crash or a reboot of the server, with `restart: unless-stopped` in the compose file.
- **To update, change the version of the image** in the compose file and run `docker compose up -d`. Pin a full version such as `0.2.0`; `latest` changes under you.

## How many chargers

One site of 2,000 chargers runs comfortably on a laptop. In the project's own test, 2,000 chargers connected in about one second, each ran a transaction with meter values, and all shut down cleanly, using under 400 MB for the site and the test CSMS together. The figures are in [docs/SPEC.md](docs/SPEC.md), section 9.

Your CSMS may want them to arrive more gently: `plugyard site up --connect-rate 50` opens at most fifty connections a second.

## What plugyard does not do

- OCPP 2.0.1; only OCPP 1.6J
- Local authorization lists and reservations; the charger answers these `NotSupported`
- Real firmware downloads; the charger reports the statuses and fetches no file
- SFTP uploads that log in with a key; a password in the location is the only way
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
- [llms.txt](llms.txt): the guide for AI agents, also printed by `plugyard agents`

## License

[Apache-2.0](LICENSE)
