# A site file's declared state wins over saved state at every power-on

A charger entry in a site file may carry a `state` block: the charger offline or stopped, a fault on a connector or on the charger, an EV plugged in. Each time the charger powers on, by any command, it is put in that declared state, whatever its saved state says. What the block does not name still comes from the saved state, as ADR 0003 describes.

We chose this for a site that is left running for others to look at, such as one on a shared staging server that must show an example of every charger state at all times (issue #39). Before, that state was set by hand after `site up` and then drifted: `offline` was forgotten at the next start, every charger was powered on, and a plugged-in EV filled up. With declared state the site file alone says what the site shows, and a restart puts it back.

## This reopens an option ADR 0006 turned down

ADR 0006 considered "a standing state in the site file" and chose the HTTP API, because a standing state helps only with states someone foresaw and gives nobody a way to act, and because it can be built on the API with a short script on a timer.

Both still hold, and the API stays the way to act. What changed is experience with the script: on a standing site it has to be written, run and looked after, and it cannot keep a charger offline or powered off across a restart, since neither is saved (issue #39). Declared state is also narrower than what ADR 0006 weighed: it is applied when a charger powers on and is not kept so between power-ons, so `exec`, a scenario and the API keep working on a declared charger.

## Considered options

- **Apply it once, when the charger is created or reset, and again when the block is edited.** This keeps ADR 0003 whole. But a hand change then outlives every restart, so the site drifts from the file, which is what the feature is for preventing.
- **Enforce it all the time.** Nothing drifts, even between restarts. But `exec` would be undone at once on a declared charger, so a developer could not try anything on one.
- **Apply it at `site up` only.** `site start` and `plugyard update` bring a site back without reading the site file, so the declaration would be lost on those. It is saved with the charger's other site-file settings instead, and needs no file.
- **Start-up steps: a scenario run when the site comes up.** It reuses the actions, but brings questions of ordering and of running twice that a declaration does not have, and `site validate` could not say what the site will show.
- **No file syntax; make more of what is set by hand persist.** `offline` and a stopped charger would survive restarts. It does not survive `site down --reset` or a move to a new machine, and the site file would still not say what the site shows.

## Consequences

- **A change made by hand lasts until the charger next powers on.** `exec <charger> online` on a charger declared offline, or `clear` on a declared fault, holds until then.
- **`plugyard restart <charger>` puts one charger back in its declared state.** A reboot and a `Reset` from the CSMS are not a power-on and apply nothing.
- **A declared EV is a new arrival at every power-on.** It takes the place of an EV in the saved state, and on a `startOn: plug` charger a transaction starts. An EV that is only saved still starts nothing by itself.
- **What the CSMS set still persists.** Configuration keys, charging profiles, meter readings and a connector the CSMS made unavailable are not part of declared state.
- **Only chargers with a `state` block are affected.** The others behave exactly as ADR 0003 says.
