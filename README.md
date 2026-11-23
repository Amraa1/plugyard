# plugyard

A terminal tool that simulates EV chargers, so you can develop and test a CSMS (charging station management system) without hardware. Its command line follows Docker's conventions: a file describes a kind of charger, a YAML file starts many of them together, and `ps`, `logs` and `exec` operate on the running ones.

> **Status: early implementation.** The V1 design is complete and written down in [docs/SPEC.md](docs/SPEC.md). So far one charger can connect to a CSMS and run transactions, driven from a second terminal. Sites, saved state and the other commands under "Planned usage" are not built yet.

## What works today

In one terminal, start a charger. `--speed 60` makes one real second count as a minute of charging:

```sh
plugyard run examples/Chargerfile --csms ws://localhost:9000/ocpp --id CP-1 --speed 60
```

It connects to `ws://localhost:9000/ocpp/CP-1`, sends `BootNotification`, a `StatusNotification` per connector and heartbeats, and prints every event and OCPP message until you press Ctrl+C. Add `--password` (or set `PLUGYARD_CSMS_PASSWORD`) for Basic Auth over `wss://`.

In a second terminal, drive a transaction:

```sh
plugyard ps                                   # the charger, its connection and connector statuses
plugyard exec CP-1 plug --soc 30%             # plug an EV into connector 1
plugyard exec CP-1 tap 04A1B2C3               # Authorize, then StartTransaction
plugyard exec CP-1 tap 04A1B2C3               # the same tag again stops it
plugyard exec CP-1 unplug
```

While the transaction runs the CSMS receives `MeterValues`, and `StopTransaction` carries the energy delivered. `exec` prints one line per charger saying whether the action was carried out, and accepts a pattern such as `'FAST-*'`.

## What V1 will do

- Simulate chargers speaking OCPP 1.6J (Core, Remote Trigger and Smart Charging) over `ws://` or `wss://` with Basic Auth
- Run one charger or a few thousand from a single YAML file
- Produce realistic meter values from a simple EV model
- Let you plug in, tap a tag, raise a fault, cut the power or drop the network on any charger
- Keep each charger's state on disk, so the same chargers come back after a stop or a reboot
- Run on Windows, Linux and macOS as a single binary

## Planned usage

Describe a kind of charger in a `Chargerfile`:

```yaml
vendor: Acme
model: FastOne 50
firmware: 1.4.2
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
```

Describe a site in `site.yaml`:

```yaml
name: depot-north
csms:
  url: ws://localhost:9000/ocpp
chargers:
  fast:
    model: ./Chargerfile
    count: 20
    id: "FAST-{n:03}"
```

Then drive it:

```sh
plugyard site up -d                      # start 20 chargers in the background
plugyard ps                              # list them
plugyard exec FAST-001 plug              # plug an EV into connector 1
plugyard exec FAST-001 tap 04A1B2C3      # present an RFID tag
plugyard logs FAST-001 -f                # follow events and OCPP messages
plugyard kill FAST-001                   # simulate a power cut
plugyard site down                       # stop; state is kept for next time
```

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
