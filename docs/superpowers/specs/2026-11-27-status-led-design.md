# Status LEDs and power colour tiers: design

Date: 2026-11-27

## Intent

The machine that runs `llama-server` should show its health with its
motherboard lighting, so the state can be read from across the room without
reading the screen. The lighting follows the dashboard's own colours: green
when nothing on the dashboard is amber or red, amber when anything is amber,
red when anything is red. The worst colour wins.

Alongside this, the power figures stop being a fixed orange and get the same
green / amber / red tiers, so they take part in the overall status.

## Goals

- One overall status level (ok, warn, bad) derived from a snapshot, by the same
  rules that colour the screen.
- The motherboard lighting shows that level: green, amber, red.
- Total power draw is coloured by its share of the power supply's wattage.
- Off by default; one setting turns it on.

## Non-goals

- Effects (blinking, breathing), per-zone colours, brightness control.
- Lighting on other devices (graphics cards, memory, keyboards).
- Installing OpenRGB from `install.sh`. The README explains how to get it.
- Talking to the LED controller directly, or to OpenRGB's network server.

## Anti-goals

- The lighting must never claim "all is well" when nothing is watching. A
  green light left on after the dashboard exits is a failure.
- The lighting must not flicker on a one-poll blip.
- Normal heavy use must not turn the lighting red. Red means something needs
  attention.
- None of the hard rules in `AGENTS.md` may be weakened.

## Constraints

From `AGENTS.md`, as they apply here:

- No new Go dependency, no CGO. The lighting is driven by running an external
  command, as `nvidia-smi` and `docker` already are.
- Local sensors only for a local server: the lighting is driven only when the
  local sources are on (loopback URL or `--gpu`), and never with `--no-gpu`.
- One-shot runs (`--once`, `--frame`) never touch the lighting.
- A missing source is normal: no `openrgb`, no matching device, or a failing
  command switches the feature off quietly, with one short note on screen.
- Never block a poll, never leak a goroutine, never grow without bound.
- Every outside string (command output, device names) is parsed tolerantly,
  stripped of control characters and capped before it reaches the terminal.
- Rendering stays pure.
- Settings a person sets are strict (bad value: exit status 2).

## Approaches

1. **Run the `openrgb` command on each colour change (chosen).** Same pattern
   as `nvidia-smi` and `docker`: no dependency, runs only when the level
   changes, works with any board OpenRGB supports. Cost: each run takes a
   moment because OpenRGB re-detects devices; acceptable because changes are
   rare.
2. **Speak OpenRGB's network protocol to its background server.** Faster to
   react, but needs a hand-written protocol client and a second service that
   must be kept running. Rejected: more code and one more thing to fail.
3. **A generic "run this command on status change" setting.** Flexible, but
   the user would have to write the lighting script. Rejected: not what was
   asked for.

The user approved approach 1 and handed over the remaining how-to choices.

## What was verified on the real machine

- The board is an ASRock TRX40 Creator. Its lighting controller is a USB HID
  device; no kernel module is needed.
- OpenRGB 1.0 detects it. `openrgb --noautoconnect --list-detailed` printed:

  ```
  0: ASRock TRX40 Creator
    Type:           Motherboard
    Description:    ASRock Polychrome USB Device
    Location:       HID: /dev/hidraw1
    Modes: Off Static Breathing Strobe 'Spectrum Cycle' Random Music Wave Spring Stack Cram Scan Neon Water [Rainbow] Direct
    Zones: 'RGB LED 1 Header' 'RGB LED 2 Header' 'Addressable Header 1' 'Addressable Header 2'
    LEDs: 'RGB LED 1 Header, LED 1' ...
  ```

  The current mode is in square brackets. Names with spaces are in single
  quotes.
- **All four zones are headers.** OpenRGB reports no on-board lights for this
  board, so only strips or fans plugged into those headers will change colour.
- OpenRGB 1.0 options used here: `--noautoconnect`, `-d` (index or name),
  `-m` (mode), `-c RRGGBB`, `--config <dir>`, `--list-detailed`.
- OpenRGB writes a settings file and one new log file per run into its
  configuration directory.
- The device node is root-only. The dashboard runs as root on that machine, so
  it can use it. A non-root dashboard needs OpenRGB's udev rules.
- Debian 12 has no `openrgb` package; the program comes from openrgb.org and
  may live outside `PATH`.

- **Colour test, watched by the user:** `openrgb --noautoconnect -d 0 -m
  Static -c <RRGGBB>` with `00FF00`, `FF7E00` and `FF0000` changed all the
  lights as expected, and `-m Rainbow` restored the previous effect. Each run
  took about 1.1 s. This was with a fresh, empty OpenRGB configuration
  directory, so a dashboard-owned `--config` directory works for this board.

**Not verified:** running OpenRGB outside a container on that machine (the
test used a throwaway container with the device passed in), and any board
other than this one.

## Design

### Overall status

- A new level type with three values: ok, warn, bad.
- A pure function in `internal/ui` returns the level for a snapshot and
  options. It does not depend on the terminal size: a panel that does not fit
  still counts.
- It must use the same thresholds the panels use. The thresholds are pulled
  into shared helpers so the screen and the status cannot drift apart.
- A test proves parity: across a set of snapshots, the status equals the worst
  warning colour the full-size screen paints, with the one exception below.

Counts as **warn**: connecting, model loading, health not "ok", GPU
temperature 80 °C or more, CPU temperature 85 °C or more, GPU memory 95% or
more, host memory 90% or more, a container that is starting, stale-data tags,
a database problem, the slots hint, total draw at 80% or
more of the power supply.

Counts as **bad**: server unreachable, GPU temperature 90 °C or more, CPU
temperature 95 °C or more, a stopped fan, a container that failed, is
unhealthy or is not running, total draw at 95% or more of the power supply.

The builder should enumerate every use of the warning and error roles in
`internal/ui` and include each one; the lists above are the ones found during
design.

**Exceptions (user's decisions):** these keep their colours on screen but do
not count towards the status:

- a missing fan (`n/a`);
- a slot's context bar (amber at 80%, red at 95%), whether the slot is
  processing the prompt or generating. A full context is normal use.

`--once` output gains one line, `status ok|warn|bad` (designer's call: plain
output states the same facts as the screen).

### Power colours

- **Total draw** (Energy panel): coloured by draw divided by the power
  supply's wattage. Under 80% green, 80% to under 95% amber, 95% and above
  red. The tier is decided on the percentage as displayed, as temperatures
  already are. The no-colour `!` / `!!` markers apply.
- With no power supply wattage set, the figure is the neutral value colour and
  has no tier.
- **Each GPU's power figure** is always green (user's decision: a card at its
  limit is normal use, not a fault).
- **The power chart** keeps its orange series colour.

### Driving the lighting

- A new package (suggested: `internal/led`) with pure parts split from the
  part that runs a command:
  - parse `--list-detailed` output into devices (index, name, type, modes);
  - build the argument list for a level;
  - run the command with a timeout.
- **Device choice.** By default, every device whose type is `Motherboard`. A
  setting can name the device instead. The device list is read once at start
  and again when a command fails.
- **Colours** (confirmed on the hardware): green `00FF00`, amber `FF7E00`,
  red `FF0000`, mode `Static`.
- **Flicker guard.** A level must be seen on three consecutive polls before
  the lighting changes. This also applies at start-up.
- **One worker.** Changes go to a single background worker; the newest level
  wins; a poll never waits for it. Each run has a timeout (suggested 20 s).
- **On exit** the lighting is switched off (mode `Off` when the device has it,
  otherwise black), with a short bounded wait. A crash or `kill -9` cannot do
  this; the README says so.
- **Failure.** If the program is missing, no device matches, or a run fails,
  the lighting is left alone and retried every 60 s, like fans. The footer
  shows one short note (suggested `led: unavailable`) in the warning colour.
  That note does not count towards the status.
- **OpenRGB's own files.** Runs use `--config` pointing at a directory the
  dashboard owns, and the dashboard keeps that directory bounded (old log
  files removed, or logging turned off if OpenRGB allows it).
- **Pause** does not affect the lighting: it follows live data.
- **Two dashboards at once** (designer's call). Only one copy drives the
  lighting: the one holding a dedicated lock file, in the way `<db>.lock`
  decides who records. The lock is separate from the database lock so it also
  works with no database. A copy without the lock never touches the lighting,
  not even on exit, and retries the lock every 30 s so it takes over when the
  holder exits.
- **The status does not depend on what one viewer selected** (designer's
  call). "History unavailable" depends on the chart range a viewer chose, so
  it stays amber on screen but does not count towards the status. This is one
  more parity exception.

### Installer

`install.sh` rebuilds the settings file from the keys it knows. It must keep
the three new settings when it rewrites the file (designer's call). It does
not ask about them.

### Wiring

- The live model computes the status after each poll and passes it to a
  callback in the options, in the way `OnScales` works today. `cmd/llamadash`
  connects that callback to the lighting controller.
- The footer note reaches the renderer through the options or the snapshot,
  so rendering stays pure.

### Settings and flags

All are person-set, so they are strict.

| Setting | Meaning | Default |
| --- | --- | --- |
| `LLAMADASH_LED` | `on` or `off` | `off` |
| `LLAMADASH_LED_CMD` | path to the `openrgb` program | `openrgb` on `PATH` |
| `LLAMADASH_LED_DEVICE` | device name to use instead of all motherboards | empty |

Matching flags (`--led`, `--led-cmd`, `--led-device`) follow the existing
pattern. Turning the feature on while local sources are off is not an error;
the lighting is simply not driven.

### Documentation

README: the "At a glance" table, "What it shows", the settings and flags
reference, and Troubleshooting (getting OpenRGB, udev rules for a non-root
dashboard, headers-only boards, lights staying on after a crash).
`docs/ARCHITECTURE.md`: the new package, the status function and the callback.

## Left to the builder

- Names of types, functions and files.
- How the thresholds are shared between the panels and the status function.
- How the parity test observes the worst colour painted.
- Exact timeout, the bounded wait on exit, and how the OpenRGB directory is
  kept small.
- Where the footer note is placed and how it is shortened on narrow screens.
- Fixture files for `--list-detailed`, including malformed ones.

## Open after the build

- Putting OpenRGB on the real machine outside a container and running the
  finished dashboard against it.
