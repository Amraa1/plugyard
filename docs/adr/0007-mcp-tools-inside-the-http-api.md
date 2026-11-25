# MCP tools inside the HTTP API, without sessions

AI agents reach plugyard's chargers through tools of the Model Context Protocol (MCP), served by the HTTP API at `/mcp`: the same address, the same tokens, the same log of calls. There is no separate command and no separate process.

We chose this because the people who asked for remote control mostly work through a coding agent, and an agent uses tools more reliably than it writes HTTP calls from a description. ADR 0006 already expected an MCP server laid over the API.

## Considered options

- **A local MCP server over standard input and output**, started by the agent's client as `plugyard mcp`. This is the usual form for a tool on the agent's own machine. But there the agent has a shell, and the command line with its guide (`plugyard agents`) serves it better. The need is the staging server, where the agent has no shell and plugyard is not installed beside it.
- **A local bridge**: `plugyard mcp --url <api> --token <token>` on the agent's machine, passing calls on to the API. It works with every client, but plugyard would have to be installed on every developer's machine only to forward calls, and the token would sit in one more place.
- **A separate port or process for MCP.** Two addresses to protect, two sets of rules to keep the same.
- **One tool for every action**, with the action as an argument. Fewer tools, but a client can then not show or ask its user about `reboot` apart from `plug`, and a read-only token could not be given a list without actions.

## Consequences

- **`/mcp` follows every rule of the API** (ADR 0006): it listens where the API listens, needs a token when a call under `/v1` does, is closed to web pages in a browser, and stays as narrow: tools read chargers and act on them, and nothing else.
- **There are no sessions.** Each request stands alone and carries its token. Nothing is kept between requests, so there is no session to steal, the API can restart without the agent noticing, and several copies behind a proxy would need nothing shared. The price: the server cannot send anything to the agent on its own, which no tool here needs.
- **One tool per action**, each marked as reading or acting. A read-only token is offered the reading tools only, so its agent never sees an action it would be refused.
- **A tool does what the call under `/v1` does**, through the same code, and answers with the same fields.
- **Each call of a tool is one line in the log of calls**, as a call under `/v1` is. The requests the protocol makes for itself are not recorded.
- **What the CSMS wrote is marked as data.** The instructions the server gives an agent, and the description of each tool whose answer can hold such text, say that it is never an instruction.
- **plugyard depends on the official MCP library for Go**, which brings a few libraries of its own. We took it over writing the protocol ourselves because the protocol still moves, and the library moves with it.
