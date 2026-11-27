# Every time in a scenario is scenario time; a caller's time is real time

A time written in a scenario follows the speed of the run: a step's `at`, and the `for` of an `offline` step. A time given by a caller stays in real time: `exec <charger> offline --for`, the same action through the API or as a tool, and `wait --timeout`. So the same `offline` action with the same `for` lasts differently from a scenario and from `exec`.

We chose this because a scenario is one timeline. Before, `at` followed the speed and `for` did not, so one file mixed two clocks: at speed 60, an outage written as two minutes lasted two real minutes, which is two hours of scenario time, and later steps ran against a charger that the file said was back. Most actions succeed on a charger that is offline, so nothing reported it (issue #36).

## Considered options

- **Shorten every `for` by the site's speed, in the charger.** `exec`, the API and the tool would change too, and a scenario step would stay exactly what `exec` does. But a run can be given a speed of its own, which the charger does not know, so `for` and `at` could still disagree. And a time typed by a person or sent by a caller is on that caller's clock, as `wait --timeout` already is.
- **Keep `for` in real time and say so.** Nothing changes, and a scenario author does the arithmetic. The same file then gives different outcomes at different speeds, which is what a scenario is meant to prevent.

## Consequences

- **The scenario runner shortens `for`** before it sends the action. The charger is told a real time and keeps one clock.
- **An outage carries only what the scenario puts in it.** What a charger sends by itself, such as `MeterValues` and `Heartbeat`, runs on real-time timers, so a short real outage queues none of it. A scenario that wants queued messages acts on the charger while it is away. Speed already does this to a transaction: thirty minutes of charging at speed 60 sends no periodic `MeterValues`.
- **A scenario has no real-time form for an outage.** A test of something the CSMS measures on its own clock, such as an alarm after five minutes out of reach, runs at speed 1, or uses `exec <charger> offline --for` beside the run. A real-time form can be added when someone needs one.
