# plugyard

plugyard simulates EV chargers that speak OCPP 1.6J, so that you can develop and test a CSMS (charging station management system) without hardware. This image is the whole program: one container runs as many chargers as your site file declares.

- **Source, documentation and issues**: [github.com/Amraa1/plugyard](https://github.com/Amraa1/plugyard)
- **Beta.** File formats and JSON output may still change before 1.0.

## Try it

With your CSMS listening on port 9000 of this machine:

```sh
docker run --rm -e CSMS_URL=ws://host.docker.internal:9000/ocpp amraa1/plugyard
```

Three chargers, `CP-1` to `CP-3`, connect to `ws://host.docker.internal:9000/ocpp/<charge point ID>`, send `BootNotification` and print everything they say and hear. `Ctrl+C` stops them.

- **On Linux**, add `--add-host host.docker.internal:host-gateway`.
- **A CSMS that asks for a password** over `wss://` gets it from `-e CSMS_PASSWORD=...`.
- **Inside a container, `localhost` is the container itself.** A CSMS on the same machine is at `host.docker.internal`.

## Drive the chargers

Everything after the image's name goes to `plugyard`, and `docker exec` runs a command in a container that is up:

```sh
docker run -d --name plugyard -e CSMS_URL=ws://host.docker.internal:9000/ocpp amraa1/plugyard
docker exec plugyard plugyard ps                      # every charger and its connectors
docker exec plugyard plugyard exec CP-1 plug          # plug an EV in
docker exec plugyard plugyard exec CP-1 tap 04A1B2C3  # present an RFID tag
docker exec plugyard plugyard wait CP-1 Charging      # returns once it charges
docker exec plugyard plugyard logs CP-1 --tail 20     # the OCPP messages, both ways
```

A charger can also report a fault, lose its connection, reboot, lose power, and fail a firmware update or a diagnostics upload. `docker exec plugyard plugyard exec --help` lists the actions.

## Your own chargers

| In the container | Holds | Give it |
|---|---|---|
| `/site` | `site.yaml` and its Chargerfiles | Your folder, mounted there. Without one, the three chargers above run |
| `/data` | Saved chargers, logs and API tokens | A volume, or the chargers start with meters at zero in every new container |
| Port `7070` | The HTTP API, when it is started | A published port, behind HTTPS |

To get a site file to start from, write the starter files into the current folder:

```sh
docker run --rm -v "$PWD:/site" amraa1/plugyard init
```

The container runs as a user that is not root, so on Linux the folder must be writable by others for this one command. The [README](https://github.com/Amraa1/plugyard#readme) says what a site file and a Chargerfile can hold.

## On a server, with the API

Others, and AI agents, can act on the chargers without a shell on the server: over HTTP, and as MCP tools at `/mcp`. A compose file for that:

```yaml
services:
  plugyard:
    image: amraa1/plugyard:0.3.0
    command: ["site", "up", "--api", "0.0.0.0:7070"]
    environment:
      CSMS_URL: ws://host.docker.internal:9000/ocpp
    extra_hosts:
      - "host.docker.internal:host-gateway"
    volumes:
      - ./:/site:ro
      - plugyard-data:/data
    ports:
      - "7070:7070"
    restart: unless-stopped
    stop_grace_period: 30s

volumes:
  plugyard-data:
```

```sh
docker compose run --rm plugyard api token create mobile-team   # prints the token, once
docker compose up -d
docker compose exec plugyard plugyard ps
```

- **Make the token first.** The API refuses to listen beyond the container's own machine until one exists.
- **Put HTTPS in front** before the port leaves the server's network. A token travels unencrypted otherwise.
- **`stop_grace_period`** gives the chargers time to end their transactions with `StopTransaction` when the container stops.

The same file, with comments, is [examples/docker/compose.yaml](https://github.com/Amraa1/plugyard/blob/main/examples/docker/compose.yaml).

## Tags

| Tag | Is |
|---|---|
| A full version, such as `0.3.0` | One release. Use this: it never changes |
| Its first two numbers, such as `0.3` | The newest release of that line |
| `latest` | The newest release |

Every release is listed with what it changes at [github.com/Amraa1/plugyard/releases](https://github.com/Amraa1/plugyard/releases).

Each is built for `linux/amd64` and `linux/arm64`. To update, change the tag and start the container again. A container is replaced by a newer image, not by `plugyard update`.

## About the image

- **No shell and no package manager**: it is the program, certificates and time zones on `distroless/static`. `docker exec plugyard sh` does not work; `docker exec plugyard plugyard <command>` does.
- **It does not run as root.**
- **`docker stop`** shuts the chargers down in the orderly way.

## For AI agents

`docker run --rm amraa1/plugyard agents` prints a guide written for agents that drive plugyard, matching the version of the image.

## License

[Apache 2.0](https://github.com/Amraa1/plugyard/blob/main/LICENSE)
