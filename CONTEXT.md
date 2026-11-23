# plugyard

A terminal tool that simulates EV chargers so developers can test a CSMS without hardware.

## Language

**Charger**:
One running simulated instance, with its own charge point ID and its own connection to the CSMS. It is what OCPP 1.6 calls a Charge Point.
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

**Charge point ID**:
The identifier by which the CSMS knows a charger.
_Avoid_: Charger ID, station ID, serial

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
