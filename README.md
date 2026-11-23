# plugyard

A terminal tool that simulates EV chargers, so you can develop and test a CSMS (charging station management system) without hardware. Its command line follows Docker's conventions: a file describes a kind of charger, a YAML file starts many of them together, and `ps` and `exec` operate on the running ones.

> **Status: early implementation.** The V1 design is written down in [docs/SPEC.md](docs/SPEC.md). Chargers can be started one at a time or as a site of many, and run transactions driven from a second terminal. Saved state, `logs`, faults and requests from the CSMS are not built yet; see [What is not built yet](#what-is-not-built-yet).

## Quick start: twenty chargers

You need a CSMS that accepts OCPP 1.6J over WebSocket. The examples assume it listens at `ws://localhost:9000/ocpp`.

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
plugyard ps                              # every charger, its connection and connector statuses
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
plugyard site down          # shuts the site down
```

The site keeps running after you close the terminal, and ends when you sign out.

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

It runs in the foreground until you press Ctrl+C. Add `--password` (or set `PLUGYARD_CSMS_PASSWORD`) for Basic Auth over `wss://`.

## What is not built yet

- Saved state: chargers start fresh every time, and `site stop`, `site start` and `ps -a` are not available
- `logs`, `stats` and `inspect`
- Actions `fault`, `clear`, `offline`, `online` and `reboot`
- Requests from the CSMS, such as `RemoteStartTransaction` and `SetChargingProfile`; the charger answers them `NotImplemented`
- `scenario run` and `init`
- Prebuilt binaries

## Building from source

Requires Go 1.26 or newer.

```sh
go build ./cmd/plugyard
./plugyard version
go test ./...
```

## Documentation

- [docs/SPEC.md](docs/SPEC.md): the V1 specification
- [CONTEXT.md](CONTEXT.md): glossary of the project's terms
- [docs/adr/](docs/adr/): records of the main design decisions

## License

[Apache-2.0](LICENSE)
