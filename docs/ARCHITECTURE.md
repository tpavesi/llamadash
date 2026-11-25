# Architecture

llamadash is one statically linked Go program. It polls a llama.cpp
`llama-server` over HTTP, reads a handful of local sensors, folds everything
into one immutable snapshot per poll, and draws that snapshot as a full-screen
terminal page. This document describes how the parts fit together and the rules
they keep. For what the program does from a user's point of view, see the
[README](../README.md).

## Design goals

These explain most of the decisions below.

- **Runs for weeks on a console.** Bounded memory everywhere (fixed-size rings,
  capped lists), no goroutine leaks, no panics on any input.
- **Small footprint.** No CGO, no GPU use, one process, about 35 MB of memory.
  Sensors are read with plain file reads or one short-lived child process.
- **Read-only towards the server.** Only `GET` requests. The dashboard never
  changes server state.
- **Private by construction.** Prompt and response text, chat templates,
  process command lines and environments, and model paths are never displayed,
  logged or stored.
- **Every input is hostile or broken.** Server JSON, `/proc`, `/sys`, command
  output and the settings file are parsed tolerantly; strings from outside are
  stripped of control characters and capped in length before they reach the
  terminal.
- **A missing source is normal.** No NVIDIA GPU, no Docker, no root, no hwmon
  driver, no database: each one removes its part of the page and nothing else.

## The data flow

```
            llama-server (HTTP GET)          local machine
        /health /props /slots /metrics   nvidia-smi  /proc  /sys  docker
                     │                        │        │     │      │
                 internal/llama        gpu  host  procstat  cpupower  hwmon  container
                     │                        │
                 internal/stats               │
                     └──────────┬─────────────┘
                          internal/collect ──────────► internal/store (SQLite)
                                │   Snapshot (one per poll)        ▲
                                ▼                                   │ history queries
                           internal/ui ─────────────────────────────┘
                                │
                     terminal (Bubble Tea)  or  --once / --frame text
```

One `Collector.Poll` call produces one `collect.Snapshot`. The UI only ever
renders a snapshot; it never talks to the server or reads a sensor itself. That
split is what makes rendering a pure function and the whole screen testable
without a terminal.

## Packages

All code is under `internal/`, with one command in `cmd/llamadash`.

| Package | Responsibility |
| --- | --- |
| `cmd/llamadash` | Flags, settings files, wiring, the database session, saving learned values back to the settings file |
| `internal/llama` | HTTP client and tolerant parsers for `/health`, `/props`, `/slots`, `/metrics` |
| `internal/stats` | Turns successive `/slots` samples into per-slot state, token rates, finished requests; fixed-size ring buffer |
| `internal/gpu` | Runs `nvidia-smi` and parses its CSV |
| `internal/cpupower` | CPU package power from the kernel's RAPL energy counters |
| `internal/host` | CPU load, memory, swap, disk from `/proc` |
| `internal/procstat` | CPU, memory and threads of the `llama-server` process from `/proc` |
| `internal/hwmon` | Motherboard fan speeds and CPU temperature from `/sys/class/hwmon` |
| `internal/container` | Docker containers via the `docker` command |
| `internal/collect` | The poll loop body: calls everything above, owns all state between polls, produces `Snapshot` |
| `internal/store` | SQLite history: samples, finished requests, per-day totals, tariff |
| `internal/cost` | Money formatting and price parsing |
| `internal/ui` | Layout, panels, charts, the Bubble Tea model, the plain-text `--once` output |
| `internal/fakeserver` | Scripted stand-in for `llama-server`, used by tests and the screenshot demo |

Dependencies point one way: sensor packages depend on nothing in the project;
`stats` depends on `llama`; `store` on `stats`; `collect` on all of those; `ui`
on `collect`; `cmd/llamadash` on `collect`, `store` and `ui`. Nothing imports
`ui` or `cmd`.

## The collector

`collect.Collector` is the only place that holds state between polls.

- **One poll, one deadline.** `Poll(ctx, now)` runs under a timeout derived
  from the refresh interval (`max(min(interval, 2s) × 0.8, 1s)`), so a slow
  server never makes polls pile up. The four server endpoints are fetched
  concurrently.
- **Reachability.** If the server does not answer, per-slot state is dropped
  and the snapshot says so; local sensors keep working. Each part of a
  snapshot carries the time it was read (`SlotsAt`, `HostAt`, `HwmonAt`, …), and
  the UI marks a part as stale from that time, not from a global flag.
- **Local sources gate.** GPU, CPU power, host, process, fans and containers
  are read only when the server URL is a loopback address or `--gpu` is given,
  and never with `--no-gpu`. A dashboard pointed at a remote server must not
  present its own machine's sensors as the server's.
- **Slow sources never block a poll.** `nvidia-smi` and `docker` are child
  processes and run on their own schedule in the background; a poll picks up
  the latest finished result. Cheap file reads (`/proc`, `/sys`) run in one
  background read guarded by a busy flag, so a hung read is started once and
  never stacks.
- **Sources that fail.** A source that is not there is switched off after it
  fails (host, process), or retried slowly (fans every 60 s, so a driver loaded
  later is picked up without a restart).
- **Energy.** Power readings are integrated over time into watt-hours. A gap
  in readings is not filled in; missing power is stored as missing, not zero.
- **History in memory.** Recent samples for the live charts are kept in rings
  of 300 samples per series, each with its start time, so a chart can state the
  span it really covers.
- **Fans.** A fan is listed once it has been seen spinning, or if it is in the
  remembered list from the settings file. From then on a reading of 0 means
  "stopped" and a missing sensor means "n/a".

## The store

`internal/store` uses the pure-Go SQLite driver (`modernc.org/sqlite`), which is
what keeps the build free of CGO.

- **One writer.** All writes go through a single goroutine that owns the write
  connection. `AddSample`, `AddRequest` and `AddUsage` only queue in memory and
  never block the poll loop. The queue is written in one transaction every 10 s
  or when 256 items are waiting. Reads use a separate read-only pool.
- **Durability.** WAL mode with `synchronous=NORMAL`. A crash loses at most the
  last few seconds; the file stays consistent.
- **One recorder per database.** The recording process holds a lock on
  `<db>.lock`. A second copy of the dashboard opens the database read-only,
  retries the lock every 30 s and takes over recording if the first one exits
  (`cmd/llamadash/db.go`).
- **Schema versions.** `schema.go` migrates in place. Version 2 added
  `daily.priced_wh`, which records how much of a day's energy has been costed,
  so energy recorded before a price was set is costed when one is.
- **Retention.** Samples and requests are pruned by age; per-day totals are
  kept.
- **One-shot runs never write.** `--once` and `--frame` read the database but
  do not record.

## The UI

Rendering is a pure function: `ui.Render(snapshot, options) string`. The
Bubble Tea `Model` in `model.go` only schedules polls, handles keys and window
sizes, fetches history off the UI goroutine, and calls `Render`.

- **Layout** (`layout.go`). Given the terminal size, a plan decides which
  panels fit and at what height. Panels are dropped in a fixed priority order
  as the screen shrinks; the header and footer always stay. Portrait and
  landscape use the same panels in different arrangements.
- **Invariants.** Every rendered line is exactly the terminal's width, there
  are never more lines than the terminal's height, there are no blank bands,
  and the footer is last. Sweep tests check these across a wide range of
  terminal sizes, in colour, 16-colour, no-colour and ASCII modes.
- **Terminal capabilities** (`render.go`). `DetectOptions` chooses between
  256-colour, 16-colour (the Linux console), `NO_COLOR` and ASCII. Colour is
  never the only signal: warning and error states also carry a `!` or `!!`
  marker when colour is off. Only glyphs the Linux console font has are used.
- **Charts** (`chart.go`, `scales.go`). One sample per column. The top of a
  chart's scale is fixed: the power supply wattage for power, the measured peak
  for token rates, 100 for percentages. `ui.Scales` lives in the model; a value
  above the top raises it for the session and reports the change so it can be
  saved.
- **Pause** freezes the displayed snapshot and its clock. Collection and
  recording continue.
- **Plain output** (`plain.go`). `--once` prints the same facts as stable
  `key value` lines for scripts; `--frame WxH` prints one rendered screen.

## Settings

Precedence, highest first: command-line flag, environment variable, the user's
file (`$XDG_CONFIG_HOME/llamadash/llamadash.conf`), the system file
(`/etc/llamadash.conf`). `--config PATH` reads exactly one file instead.

There are two kinds of keys:

- **Set by a person** (`LLAMADASH_URL`, `LLAMADASH_KWH_PRICE`,
  `LLAMADASH_PSU_WATTS`, …). An unknown key or a bad value is an error and the
  program exits with status 2. The systemd unit does not restart on status 2,
  so a typo does not cause a restart loop.
- **Maintained by the program** (`LLAMADASH_AXIS_*`, `LLAMADASH_KNOWN_FANS`).
  These are read leniently: a bad value is ignored with a warning and
  overwritten at the next save, because a value the program wrote must never be
  able to stop the program from starting.

Writing back (`confwrite.go`, `scalesave.go`) re-reads the file, replaces only
the program's own lines, and renames a fully written temporary file over the
original with the same permissions. Writes are coalesced to at most one per
30 seconds, and a failed write backs off for 10 minutes without disturbing the
display.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | normal exit |
| 1 | runtime failure (in `--once`: the server did not answer) |
| 2 | bad flag, bad setting or unreadable explicit settings file |

## Build and release

`python build.py` runs the tests and cross-compiles `linux/amd64` and
`linux/arm64` with `CGO_ENABLED=0` into `dist/`, alongside `install.sh` and
`contrib/`. The version string comes from `git describe`. `install.sh` is a
POSIX shell script that asks for each setting, validates the answers by running
the binary itself, and writes nothing until the summary is confirmed.

## Tests

- Parsers are tested against fixtures in `testdata/`: a real capture from a
  `llama-server` (scrubbed of text content), hand-written `/proc`, `/sys`,
  `nvidia-smi` and `docker` output, and deliberately malformed input.
- The collector is tested against `internal/fakeserver` with an injected clock;
  no test sleeps for real time beyond a few tens of milliseconds.
- The UI is tested by rendering snapshots to strings and asserting on the text,
  plus the layout sweeps described above.
- Linux-only code (file locks, disk statistics, file ownership) sits behind
  build tags with a stub for other systems, so the suite also runs on Windows
  and macOS. The full suite is run under the race detector on Linux.
