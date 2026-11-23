# Charger state persists until explicitly reset

A charger's state (configuration keys, charging profiles, meter readings, open transaction, queued messages) is saved to disk and survives `site stop`, `site down`, terminal exits and machine shutdowns. Only `site down --reset` or `rm` wipes it. We chose this over factory-fresh chargers on every `site up` because a developer testing a CSMS wants to return to the same chargers the CSMS already knows, with the same configuration and a meter that never runs backwards, without re-provisioning them each session.

## Considered options

- **State in memory only, discarded on `site down`.** Simpler and gives a known starting point for every run, but forces the user to rebuild CSMS-side state each time. Repeatable runs are still available through `--reset`.

## Consequences

- Site files and saved state can disagree. Saved state wins for anything the CSMS changed; structural edits to a site file or Chargerfile reset only the affected chargers, after confirmation.
- A machine shutdown during a transaction is indistinguishable from a power cut, so the charger reports the interrupted transaction with reason `PowerLoss` on the next start. `plugyard kill` exposes the same path on demand.
- Site names must be unique per user, because they key the saved state.
