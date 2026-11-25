# AGENTS.md

Guidance for AI coding agents (and people) working in this repository. Read
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) first: it explains the data flow
and the rules each package keeps.

## What this is

`llamadash` is a terminal dashboard for a llama.cpp `llama-server`. One Go
module (`llamadash`), one command (`cmd/llamadash`), everything else under
`internal/`. It targets Linux (amd64, arm64) and is developed on any system.

## Commands

```
go test ./...                 # all tests; must pass on Windows, macOS and Linux
go vet ./...
gofmt -l .                    # must print nothing
python build.py               # tests + Linux binaries into dist/
```

Race detector, on Linux (use a container elsewhere):

```
docker run --rm -v "$PWD:/src:ro" -w /src -e GOFLAGS=-buildvcs=false \
  -e GOCACHE=/tmp/gc golang:1.27 go test -race -count=1 ./...
```

Try it without a server: the tests use `internal/fakeserver`; a single rendered
screen from any server is `llamadash --frame 135x60 --url http://host:port`.

Screenshots in `docs/screenshots/` are generated from demo data, never from a
real machine:

```
LLAMADASH_SHOTS=out go test -run TestWriteScreenshots ./internal/ui/
python tools/screenshots.py
```

## Hard rules

These are product guarantees. Do not weaken them, even to make a feature
easier.

1. **Read-only towards the server.** Only `GET` requests to `/health`,
   `/props`, `/slots`, `/metrics`. Never `POST`, never slot save, restore or
   erase.
2. **No text content.** Never display, log or store prompt or response text,
   `chat_template`, `generation_prompt`, `media_marker`, process command lines
   or environments, or full model paths. Fixtures must be scrubbed the same way
   (see `testdata/README.md`).
3. **No CGO, no new dependencies** without a strong reason. The build is
   `CGO_ENABLED=0`; SQLite is the pure-Go driver.
4. **Never panic, never grow without bound, never leak a goroutine.** The
   program runs for weeks. Every list is capped and every history is a
   fixed-size ring.
5. **Treat every input as broken.** Server JSON, `/proc`, `/sys`, command
   output and the settings file: parse tolerantly, strip control characters and
   cap the length of any outside string before it reaches the terminal.
6. **A missing source is normal.** No GPU, no Docker, no root, no hwmon driver,
   no database: remove that part of the page and nothing else.
7. **Local sensors only for a local server.** They are read when the URL is a
   loopback address or `--gpu` is given, and never with `--no-gpu`.
8. **One-shot runs write nothing.** `--once` and `--frame` never write the
   database or the settings file.
9. **A value the program wrote must not stop it starting.** Keys maintained by
   the program (`LLAMADASH_AXIS_*`, `LLAMADASH_KNOWN_FANS`) are read leniently;
   keys a person sets are strict and exit with status 2.

## Where things go

| To change… | Look in |
| --- | --- |
| What is fetched from the server, or how it is parsed | `internal/llama` |
| Per-slot state, token rates, finished requests | `internal/stats` |
| A local sensor | its own package (`gpu`, `cpupower`, `host`, `procstat`, `hwmon`, `container`) |
| When sources are read, state between polls, the `Snapshot` | `internal/collect` |
| What is stored and how it is queried | `internal/store` (schema changes go in `schema.go` as a new version) |
| How anything looks | `internal/ui` (`panels*.go`, `chart.go`, `layout.go`, `plain.go` for `--once`) |
| Flags and settings keys | `cmd/llamadash/main.go`, `config.go` |
| The installer | `install.sh` (POSIX `sh`, tabs, LF line endings) |

Adding a new source usually means: a small package that only parses, with its
Linux-only reads split from pure parsing; a reader field in `collect.Config` so
tests can inject one; fields in `Snapshot` with the time they were read; a panel
or row in `internal/ui`; a line in `plain.go`; and a row in the README.

## Conventions

- **Tests first.** Write the failing test, then the code. Parsers get fixture
  files under `testdata/`, including malformed ones.
- **No real time in tests.** Inject the clock and intervals; no sleep longer
  than about 50 ms. Tests never touch `/etc`, the real home directory or the
  network (use `192.0.2.x` when an unreachable address is needed).
- **Cross-platform tests.** Linux-only code goes in `*_linux.go` with a
  `*_other.go` stub, so `go test ./...` passes everywhere.
- **Rendering is pure.** `ui.Render(snapshot, options)` takes no clock and
  reads nothing. New screen state belongs in the snapshot or the model.
- **Layout invariants.** Every line is exactly the terminal width, never more
  lines than the height, no blank bands, footer last. The sweep tests enforce
  this; extend their snapshots when adding a row or panel.
- **Colour is never the only signal.** Use the renderer's roles, and the `!` /
  `!!` markers for warning and error in no-colour modes. Use only glyphs the
  Linux console font has (box drawing, `█`, `░`, `■`).
- **Comments** say why, not what. Keep them short.
- **Line endings.** `install.sh`, `contrib/*` and sysfs-style fixtures are LF
  (see `.gitattributes`).

## Documentation

When behaviour changes, update the README in the same change: the "At a
glance" table, "What it shows", the settings and flags reference, and
Troubleshooting where relevant. Update `docs/ARCHITECTURE.md` when a rule or
the data flow changes.

## Before you finish

- `gofmt -l .` prints nothing, `go vet ./...` is clean, `go test ./...` passes.
- The race suite passes on Linux for changes to `collect`, `store`, `ui` or
  `cmd/llamadash`.
- No fixture, test or document contains a real host name, address, user name,
  model file name or container name from a real machine.
- State plainly what was not verified (for example, behaviour on real hardware
  you did not have).
