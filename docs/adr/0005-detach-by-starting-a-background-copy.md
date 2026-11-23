# `site up -d` detaches by starting a background copy of plugyard

`plugyard site up -d` starts the same `plugyard` program again as a background process that is cut loose from the terminal, waits until that process answers on the site's socket, reports, and exits. The mechanism is the same on Windows, Linux and macOS; only the instruction that cuts the child loose differs. On Linux and macOS the child starts in a new session with no controlling terminal, so the hang-up sent when the terminal closes never reaches it. On Windows it starts with no console attached and in its own process group.

We chose this because it is the only option that gives the same `-d` on all three systems with nothing to install, and it keeps the no-daemon model of ADR 0001.

## Considered options

- **Hand the process to the system's service manager** (systemd, launchd, Task Scheduler or a Windows service). It would survive signing out and could restart after a crash, but it means three implementations, needs registration and sometimes administrator rights, is missing on Linux systems without systemd, and brings back the "cannot reach the service" failures that ADR 0001 avoids.
- **No `-d`; users detach it themselves** with `nohup`, `tmux` or `Start-Process`. No code, but a different recipe per system, a poor experience on Windows, and scripts that are not portable.
- **A separate console window** for the background copy. Closing the window would kill the site, and it has no equivalent on a server or in CI.
- **A permanent background service that owns every site**, as Docker has. Rejected in ADR 0001.
- The classic Unix "fork into the background" is not available: Go programs cannot safely fork without starting a new program, and Windows has no equivalent.

## Consequences

- The background process has no terminal, so what it would print goes to a log file beside the site's socket in the data directory. If it fails to start, `site up -d` shows the end of that file.
- Nothing restarts the process if it crashes.
- The site ends when the user signs out, on Windows and macOS always and on Linux where the system is set to end user processes at logout. This matches the rule that sites do not start by themselves after a reboot.
- `plugyard site down` asks the process to shut down over its socket, since there is no terminal to press Ctrl+C in.
- An optional `plugyard service install` can still be added later for users who need a site to outlive their session.
