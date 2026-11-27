# A site file's declared state wins over saved state at every power-on

A charger entry in a site file may carry a `state` block: the charger offline or stopped, a fault on a connector or on the charger, an EV plugged in. Each time the charger powers on, by any command, it is put in that declared state, whatever its saved state says. What the block does not name still comes from the saved state, as ADR 0003 describes.

We chose this for a standing fleet, such as a shared staging server that must show an example of every charger state at all times (issue #39). Before, that state was set by hand after `site up` and then drifted: `offline` was forgotten at the next start, every charger was powered on, and a plugged-in EV filled up. With declared state the site file alone says what the fleet shows, and a restart puts it back.

## Considered options

- **Apply it once, when the charger is created or reset, and again when the block is edited.** This keeps ADR 0003 whole. But a hand change then outlives every restart, so the fleet drifts from the file, which is what the feature is for preventing.
- **Enforce it all the time.** Nothing drifts, even between restarts. But `exec` would be undone at once on a declared charger, so a developer could not try anything on one.
- **Apply it at `site up` only.** `site start` and `plugyard update` bring a site back without reading the site file, so the declaration would be lost on those. It is kept with the charger's snapshot of its site-file settings instead, and needs no file.
- **Start-up steps: a scenario run when the site comes up.** It reuses the actions, but brings questions of ordering and of running twice that a declaration does not have, and `site validate` could not say what the fleet will show.
- **No file syntax; make more of what is set by hand persist.** `offline` and a stopped charger would survive restarts. It does not survive `site down --reset` or a move to a new machine, and the site file would still not say what the fleet shows.

## Consequences

- **A change made by hand lasts until the charger next powers on.** `exec <charger> online` on a charger declared offline, or `clear` on a declared fault, holds until then.
- **`plugyard restart <charger>` puts one charger back in its declared state.** A reboot and a `Reset` from the CSMS are not a power-on and apply nothing.
- **A declared EV is a new arrival at every power-on.** It takes the place of an EV in the saved state, and on a `startOn: plug` charger a transaction starts. An EV that is only saved still starts nothing by itself.
- **What the CSMS set still persists.** Configuration keys, charging profiles, meter readings and a connector the CSMS made unavailable are not part of declared state.
- **Only chargers with a `state` block are affected.** The others behave exactly as ADR 0003 says.
