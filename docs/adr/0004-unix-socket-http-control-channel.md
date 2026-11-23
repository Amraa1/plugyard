# Commands reach a running site over a Unix domain socket carrying HTTP and JSON

ADR 0001 has commands such as `ps` and `exec` talk to the process that owns the chargers over a local socket. That socket is a Unix domain socket file on all three operating systems, one per running site, in the per-user data directory and readable only by that user. Requests and answers are HTTP with JSON bodies, as the Docker CLI does with its own socket.

We chose a Unix domain socket over a Windows named pipe because Windows 10 and later support them, which gives one code path for every platform. We chose it over a localhost TCP port because a file needs no port allocation, cannot clash with other software, raises no firewall prompt, and is protected by file permissions. We chose HTTP and JSON over a custom protocol because the standard library provides the client, the server, streaming for `logs -f`, and error codes.

## Consequences

- The spec says V1 has no HTTP API. That still holds: this channel is internal, undocumented and free to change between versions. Every request carries the protocol version, and a command built for a different version fails with a clear message rather than misbehaving.
- Running sites are discovered by listing the socket files. A socket left behind by a process that died is detected when connecting to it fails, and is removed.
- Socket paths have a short length limit (about 100 characters), so socket files sit directly in the data directory under short names.
- Windows versions older than Windows 10 1803 are not supported.
