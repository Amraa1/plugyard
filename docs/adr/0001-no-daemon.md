# No daemon: the `site up` process owns its chargers

plugyard copies Docker's command line but not its architecture. There is no background service; the process started by `plugyard site up` holds the chargers, `-d` detaches it from the terminal, and commands such as `ps`, `logs` and `exec` reach it over a local socket. We chose this because a daemon must be installed and kept running as a system service in three different ways on Windows, Linux and macOS, and "cannot connect to the daemon" is a class of failure we do not want users to meet.

## Consequences

- Sites do not come back by themselves after a reboot. The user runs `plugyard site up -d` again; saved state makes the chargers resume where they were (see ADR 0003).
- No single process owns all state, so storage must tolerate one writer and other readers at the same time (see ADR 0002).
- An optional `plugyard service install` can be added later without changing this model.
