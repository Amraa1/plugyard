# plugyard

A terminal tool that simulates EV chargers so developers can test a CSMS without hardware.

## Language

**Charger**:
One simulated instance, with its own charge point ID and its own connection to the CSMS. It exists whether powered on or stopped. It is what OCPP 1.6 calls a Charge Point.
_Avoid_: Charge point, charging station, container, instance

**Model**:
A kind of charger: its vendor, model name, firmware version, connectors, maximum power and default configuration. Many chargers can be instantiated from one model.
_Avoid_: Image, template, profile, type

**Chargerfile**:
The file that declares one model.
_Avoid_: Model file, definition file

**Site**:
A named group of chargers that are started and stopped together.
_Avoid_: Project, stack, fleet, group

**Connector**:
A numbered outlet on a charger that one vehicle can plug into, as in OCPP 1.6.
_Avoid_: Plug, socket, port, EVSE

**EV**:
The simulated vehicle drawing energy from a connector. It exists only while plugged in and has no identity of its own.
_Avoid_: Car, vehicle, battery

**EV preset**:
A named set of EV parameters that a plug-in can refer to.
_Avoid_: EV profile, EV model, vehicle type

**Scenario**:
A sequence of timed actions applied to the chargers of a running site.
_Avoid_: Script, test, playbook

**Speed**:
How many times faster than real time the EVs of a site charge. A scenario runs at the speed of its site unless the run is given its own.
_Avoid_: Time scale, multiplier, acceleration, rate

**Scenario time**:
Time as a scenario writes it, counted from the start of the run. It passes faster than real time by the speed of the run.
_Avoid_: Simulated time, virtual time, step time

**Charge point ID**:
The identifier by which the CSMS knows a charger.
_Avoid_: Charger ID, station ID, serial

**Name**:
A second handle for a charger, used on the command line in place of its charge point ID and unique on the machine. The CSMS never sees it.
_Avoid_: Alias, label, nickname

**Saved state**:
What a charger remembers while it is powered off, so that it comes back as the same charger.
_Avoid_: Snapshot, persistence, volume

**Declared state**:
The state a site file says a charger is in each time it powers on, such as offline, faulted or with an EV plugged in. Where it speaks, it takes the place of the saved state.
_Avoid_: Starting state, initial state (it holds at every power-on, not only the first), desired state (nothing enforces it between power-ons)

**CSMS**:
The backend system under test that chargers connect to and exchange OCPP messages with.
_Avoid_: Central system, backend, server

**Transaction**:
One period of charging on a connector, from the moment the CSMS is told it started to the moment it is told it stopped, as in OCPP 1.6.
_Avoid_: Session, charging session, charge

**Tag**:
The identifier a driver presents to a charger to start or stop a transaction. OCPP 1.6 carries it in the `idTag` field.
_Avoid_: Card, RFID, token, badge

**Action**:
Something done to a running charger from outside, such as plugging in an EV, presenting a tag or raising a fault.
_Avoid_: Command, operation, event

**Fault**:
Something wrong with a connector or with the charger as a whole, reported to the CSMS with an OCPP error code until it is cleared.
_Avoid_: Error (an error is an answer to a message), failure, alarm

**Charging profile**:
A limit over time that the CSMS puts on charging, for one transaction, for every transaction, or for the charger as a whole.
_Avoid_: Schedule (a schedule is the part of a profile that holds the limits), power limit, throttle

**Charge curve**:
The share of its maximum power that an EV accepts at each state of charge.
_Avoid_: Power curve, charging profile (an OCPP term for a limit the CSMS sets)

**ID template**:
The text in a site file from which the charge point IDs of an entry's chargers are made, such as `FAST-{n:03}`.
_Avoid_: ID pattern (a pattern selects chargers in a command), naming scheme

**API**:
The HTTP interface through which a caller with no shell on the machine acts on chargers and reads them. It offers the actions and nothing more.
_Avoid_: REST API (it is not a full one), control API, remote control (a remote start or stop is something the CSMS does)

**API token**:
The secret a caller of the API sends to show it may call, known by a name that the log of calls shows.
_Avoid_: Token alone where a tag could be meant, key, password, credential

**Tool**:
One reading or one action of the API as an AI agent calls it, over the Model Context Protocol (MCP). A tool either reads chargers or acts on them.
_Avoid_: Function, command (a command is typed on the command line), endpoint

