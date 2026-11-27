# Status LEDs and Power Colour Tiers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The motherboard lighting shows the dashboard's worst colour (green, amber, red), and total power draw is coloured by its share of the power supply's wattage.

**Architecture:** A pure function in `internal/ui` turns a snapshot into one level using the same helpers that colour the panels. The live model reports that level after every poll through a callback; `cmd/llamadash` connects the callback to a controller in a new `internal/led` package, which debounces the level and runs the `openrgb` command in one background worker.

**Tech Stack:** Go (module `llamadash`), Bubble Tea, the external `openrgb` 1.0 command. No new Go dependency.

**Spec:** `docs/superpowers/specs/2026-11-27-status-led-design.md`. Read it and `AGENTS.md` before any task.

## Global Constraints

- No new Go dependency, no CGO. `go test ./...` must pass on Windows, macOS and Linux; Linux-only code goes in `*_linux.go` with a `*_other.go` stub.
- `gofmt -l .` prints nothing and `go vet ./...` is clean at the end of every task.
- No real time in tests: inject the clock; no sleep longer than about 50 ms. Tests never touch `/etc`, the real home directory or the network.
- Rendering is pure: `ui.Render` and `ui.Status` read nothing and take no clock other than `Options.Now`.
- Never block a poll, never leak a goroutine, never grow without bound. Every list is capped.
- Every outside string (command output, device names) is stripped of control characters and capped before use. Never log or display a command line.
- `--once` and `--frame` never run `openrgb` and never write anything.
- The lighting is driven only when local sources are on (loopback URL or `--gpu`) and never with `--no-gpu`.
- Person-set settings are strict: a bad value prints one line to stderr and exits with status 2.
- Thresholds, verbatim from the spec: GPU temperature warn 80, bad 90; CPU temperature warn 85, bad 95; GPU memory warn 0.95; host memory warn 0.9; power draw warn 80%, bad 95% of the power supply.
- Colours: green `00FF00`, amber `FF7E00`, red `FF0000`, mode `Static`.
- Flicker guard: 3 consecutive polls. Command timeout: 20 s. Retry after failure: 60 s. Lock retry: 30 s. Bounded wait on exit: 3 s.
- `install.sh` and `contrib/*` keep LF line endings; `install.sh` is POSIX `sh` with tabs.
- No fixture, test or document contains a real host name, address, user name, model file name or container name.
- Commit after each task with a conventional message (`feat:`, `docs:`), on a feature branch, not `main`.

## Review Focus

1. `openrgb` hangs and never exits: the poll loop keeps running, the run is killed after the timeout, and quitting the dashboard still returns within the bounded wait. (Task 4)
2. The level alternates every poll (ok, warn, ok, warn): the lighting never changes and no command is run. (Task 4)
3. `openrgb --list-detailed` prints garbage, nothing, megabytes, or names with control characters: no panic, no device, the footer note appears. (Tasks 3 and 4)
4. A second dashboard copy starts and quits while the first runs: the second never runs `openrgb`, not even on exit, so the lights stay on. (Task 4)
5. `openrgb` is missing at start and appears later, or one run fails: the current level is applied on the next retry without waiting for a level change. (Task 4)

---

### Task 1: Power colour tiers

**Files:**
- Create: `internal/ui/status.go`
- Modify: `internal/ui/panels.go` (GPU power figure near line 351; Energy "draw" near line 374)
- Test: `internal/ui/status_test.go`, plus existing tests and sweep snapshots that assert the old colour
- Modify: `README.md` ("What it shows", "GPU and CPU power")

**Interfaces:**
- Produces: `func powerRole(powerW, psuWatts float64) role` in `status.go`.

- [ ] **Step 1: Write the failing tests** in `status_test.go`:

```go
func TestPowerRole(t *testing.T) {
	for _, c := range []struct {
		w, psu float64
		want   role
	}{
		{500, 0, rValue},     // no power supply wattage: neutral, no tier
		{790, 1000, rGood},   // 79%
		{794.9, 1000, rGood}, // shows as 79%
		{795, 1000, rWarn},   // shows as 80%: the tier follows the figure shown
		{944, 1000, rWarn},   // 94%
		{949, 1000, rBad},    // shows as 95%
		{950, 1000, rBad},
		{1200, 1000, rBad},
	} {
		if got := powerRole(c.w, c.psu); got != c.want {
			t.Errorf("powerRole(%v, %v) = %v, want %v", c.w, c.psu, got, c.want)
		}
	}
}
```

Add two render tests: with `PSUWatts: 1000, PowerW: 960, HasPower: true` the Energy panel's draw figure in `NoColor` mode reads `960 W!!`; with `PowerW: 850` it reads `850 W!`. Add one test that a GPU's power figure is painted with `rGood` (compare against `r.paint(rGood, "250 W")` in 256-colour mode).

- [ ] **Step 2: Run** `go test ./internal/ui/ -run 'TestPowerRole|Energy|GPU'`. Expected: FAIL, `powerRole` undefined.

- [ ] **Step 3: Implement** `powerRole` in `status.go`: `psuWatts <= 0` returns `rValue`; otherwise the tier is decided on `math.Round(powerW/psuWatts*100)` with 80 and 95. In `panels.go` paint each GPU's power with `rGood`, and the Energy draw with `powerRole(s.PowerW, s.PSUWatts)` followed by `r.warnMark` of that role. The power chart keeps `rPower`.

- [ ] **Step 4: Run** `go test ./internal/ui/`. Update snapshots and assertions that encoded the orange figure. Expected: PASS.

- [ ] **Step 5: README.** In "What it shows" and "GPU and CPU power", state the tiers (green under 80%, amber from 80%, red from 95% of `LLAMADASH_PSU_WATTS`; neutral when it is not set; each GPU's own figure is green).

- [ ] **Step 6: Commit** `feat: colour total power draw by its share of the power supply`.

---

### Task 2: Overall status level

**Files:**
- Modify: `internal/ui/status.go`, `internal/ui/panels.go`, `internal/ui/panels_local.go`, `internal/ui/containers.go`, `internal/ui/render.go`, `internal/ui/plain.go`
- Test: `internal/ui/status_test.go`, `internal/ui/plain_test.go`
- Modify: `README.md` (the `--once` description)

**Interfaces:**
- Consumes: `powerRole` (Task 1).
- Produces:

```go
// Level is the worst state on the dashboard.
type Level int

const (
	LevelOK Level = iota
	LevelWarn
	LevelBad
)

func (l Level) String() string // "ok", "warn", "bad"

// Status is the level of s as the full dashboard would colour it. It uses
// o.Now and o.Interval (for stale tags) and nothing else from o.
func Status(s collect.Snapshot, o Options) Level
```

- [ ] **Step 1: Write the failing tests.** In `status_test.go`:

A helper `paintedLevel(out string) Level` that returns `LevelBad` when the rendered text contains the escape sequence of `rBad` in the 256-colour palette (`38;5;203`), else `LevelWarn` for `rWarn` (`38;5;221`), else `LevelOK`.

`TestStatusMatchesScreen`: a table of snapshots, each rendered with `Render(s, Options{Width: 220, Height: 140, Now: now, Interval: time.Second})` (256-colour, not paused). For every row, `Status(s, o)` must equal `paintedLevel(Render(s, o))`. Rows, one each, on top of a healthy base snapshot (reachable, health ok, one idle slot, one GPU at 50 °C, host, fans spinning, a healthy container, `PSUWatts: 1000, PowerW: 300`):

| Row | Expected |
| --- | --- |
| base | ok |
| zero snapshot (`At` zero: connecting) | warn |
| `Reachable: false` | bad |
| `Loading: true` | warn |
| `Health.Status: "degraded"` | warn |
| GPU `TempC: 79.6` | warn |
| GPU `TempC: 90` | bad |
| GPU memory 95% used | warn |
| `CPUTempC: 85` | warn |
| `CPUTempC: 95` | bad |
| host memory 90% used | warn |
| a fan with `Stopped: true` | bad |
| `Container.Health: "starting"` | warn |
| `Container.Health: "unhealthy"` | bad |
| `Container.State: "exited"` | bad |
| a `Containers` entry that maps to `container.LevelStarting` | warn |
| a `Containers` entry that maps to `container.LevelFailed` | bad |
| `DBStatus: "off (disk full)"` | warn |
| `SlotsHint: "slots endpoint disabled"` | warn |
| `GPUAt` ten intervals old (stale panel) with GPU `TempC: 95` | warn |
| `PowerW: 850` | warn |
| `PowerW: 960` | bad |

`TestStatusExceptions`: for each of these the screen shows amber or red (`paintedLevel` is not ok) but `Status` is `LevelOK`:
- a fan with `Missing: true`;
- a busy slot (`stats.Generating`) with `CtxUsed/NCtx` = 0.85, one with 0.97, and the same two with `stats.PromptEval`;
- `Options{LongErr: true, Range: Range1h, History: true}` on the base snapshot.

In `plain_test.go`: `RenderPlain` of the base snapshot contains the line `status ok`; with `Reachable: false` it contains `status bad`.

- [ ] **Step 2: Run** `go test ./internal/ui/ -run 'TestStatus|Plain'`. Expected: FAIL, `Status` undefined.

- [ ] **Step 3: Implement.** `Status` builds a renderer (`newRenderer(s, o)`) and calls an unexported `func (r *renderer) level() Level`, so stale tags and visibility rules come from the renderer's own methods. Move each threshold decision into one small helper in `status.go` that both the panel code and `level()` call (temperature role with its two limits, GPU memory, host memory, header health, container state and health roles, container list level to role). Enumerate every `rWarn` and `rBad` in `internal/ui` (`grep -n 'rWarn\|rBad'`) and make `level()` cover each one except: the missing fan, the slot context bar, the chart's "history unavailable" tag, and the footer note of Task 5. A stale panel counts as warn and its dimmed content does not count. Data that is absent (no GPU, no container, no host) counts for nothing. `RenderPlain` gains the line `status <level>` using `Options{Now: s.At, Interval: time.Second}`.

- [ ] **Step 4: Run** `go test ./internal/ui/`. Expected: PASS, including the layout sweeps.

- [ ] **Step 5: README.** Mention the `status` line where `--once` output is described.

- [ ] **Step 6: Commit** `feat: one overall status level for the dashboard`.

---

### Task 3: OpenRGB device listing and arguments

**Files:**
- Create: `internal/led/led.go`
- Create: `testdata/openrgb/list_detailed.txt`, `list_two_devices.txt`, `list_garbage.txt`, `list_empty.txt`
- Modify: `testdata/README.md` (one line describing the new directory)
- Test: `internal/led/led_test.go`

**Interfaces:**
- Produces:

```go
package led

type State int

const (
	Off State = iota
	Green
	Amber
	Red
)

type Device struct {
	Index int
	Name  string   // cleaned, at most 64 bytes
	Type  string   // e.g. "Motherboard"
	Modes []string // mode names without quotes or brackets, at most 64
}

// ParseDevices reads `openrgb --list-detailed` output. Unusable input gives no devices. At most 32 devices.
func ParseDevices(out []byte) []Device

// Pick returns the devices to drive: with name == "", those whose Type is
// "Motherboard" (case-insensitive); otherwise those whose Name contains name (case-insensitive).
func Pick(devs []Device, name string) []Device

// Args is the openrgb argument list that puts every device in st.
func Args(devs []Device, st State, configDir string) []string

// ListArgs is the argument list that lists devices.
func ListArgs(configDir string) []string
```

- [ ] **Step 1: Fixtures.** `list_detailed.txt` is exactly:

```
0: ASRock TRX40 Creator
  Type:           Motherboard
  Description:    ASRock Polychrome USB Device
  Location:       HID: /dev/hidraw1
  Modes: Off Static Breathing Strobe 'Spectrum Cycle' Random Music Wave Spring Stack Cram Scan Neon Water [Rainbow] Direct
  Zones: 'RGB LED 1 Header' 'RGB LED 2 Header' 'Addressable Header 1' 'Addressable Header 2'
  LEDs: 'RGB LED 1 Header, LED 1' 'RGB LED 2 Header, LED 1'
```

`list_two_devices.txt` adds, before it, a line starting `[54        ][Dialog ]<h2>Warning:</h2>` (OpenRGB prints such lines; they are skipped) and, after it, a second device `1: Example Keyboard` with `Type: Keyboard` and `Modes: Direct Static` (no `Off`). `list_garbage.txt` holds a device line with an ESC character in the name, a 300-character name, a line `x: no index`, and 40 well-formed devices. `list_empty.txt` is empty. All LF.

- [ ] **Step 2: Write the failing tests:**

```go
func TestParseDevices(t *testing.T) {
	d := ParseDevices(read(t, "list_detailed.txt"))
	// one device: Index 0, Name "ASRock TRX40 Creator", Type "Motherboard",
	// Modes contains "Off", "Static", "Spectrum Cycle" and "Rainbow" (no quotes, no brackets)
}
func TestParseDevicesBroken(t *testing.T) {
	// list_empty.txt -> none; list_garbage.txt -> at most 32 devices, every Name
	// at most 64 bytes with no byte below 0x20, no device for "x: no index"
}
func TestPick(t *testing.T) {
	// two devices: Pick(devs, "") -> only index 0; Pick(devs, "keyboard") -> only index 1; Pick(devs, "nothing") -> none
}
func TestArgs(t *testing.T) {
	devs := ParseDevices(read(t, "list_two_devices.txt"))
	// Args(devs[:1], Green, "/x") == {"--noautoconnect", "--config", "/x", "-d", "0", "-m", "Static", "-c", "00FF00"}
	// Amber -> "FF7E00"; Red -> "FF0000"
	// Args(devs[:1], Off, "/x") ends "-d", "0", "-m", "Off"
	// Args(devs[1:], Off, "/x") ends "-d", "1", "-m", "Static", "-c", "000000"   (device without an Off mode)
	// Args(devs, Red, "/x") holds one "-d ... -m ... -c ..." group per device, in order
	// ListArgs("/x") == {"--noautoconnect", "--config", "/x", "--list-detailed"}
}
```

- [ ] **Step 3: Run** `go test ./internal/led/`. Expected: FAIL, package has no code.

- [ ] **Step 4: Implement** in `led.go`. A device starts at a line matching `^\d+: name`; the indented `Type:` and `Modes:` lines that follow belong to it. Mode tokens are space-separated; a token in single quotes may contain spaces; square brackets around the current mode are removed.

- [ ] **Step 5: Run** `go test ./internal/led/`. Expected: PASS.

- [ ] **Step 6: Commit** `feat: parse openrgb device listings and build its arguments`.

---

### Task 4: Lighting controller

**Files:**
- Create: `internal/led/controller.go`, `internal/led/run.go`, `internal/led/prune.go`
- Test: `internal/led/controller_test.go`, `internal/led/prune_test.go`

**Interfaces:**
- Consumes: `ParseDevices`, `Pick`, `Args`, `ListArgs`, `State` (Task 3).
- Produces:

```go
// Runner runs openrgb with args and returns its output (capped by the implementation).
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// ErrUnavailable means the openrgb program was not found.
var ErrUnavailable = errors.New("led: openrgb not available")

// Command returns a Runner for the program at path (looked up on PATH when it has no separator).
// Output is capped at 64 KiB; WaitDelay is one second, as in internal/gpu.
func Command(path string) Runner

type Config struct {
	Run        Runner
	Device     string                    // "" = every motherboard
	ConfigDir  string                    // openrgb's --config directory; created 0700 when missing
	Lock       func() (io.Closer, error) // nil: always the owner. An error means another copy owns the lights.
	Now        func() time.Time          // nil: time.Now
	Hold       int                       // 0 -> 3
	Timeout    time.Duration             // 0 -> 20s
	RetryEvery time.Duration             // 0 -> 60s
	LockEvery  time.Duration             // 0 -> 30s
	ExitWait   time.Duration             // 0 -> 3s
}

type Controller struct{ /* unexported */ }

func New(cfg Config) *Controller

// Offer reports the level of one poll. It never blocks.
func (c *Controller) Offer(st State)

// Note is "" while the lights work or belong to another copy, else "led: unavailable".
func (c *Controller) Note() string

// Close switches the lights off if this copy changed them, waits at most ExitWait, and stops the worker.
func (c *Controller) Close()

// PruneLogs removes all but the newest keep regular files in dir. A missing dir is not an error.
func PruneLogs(dir string, keep int) error
```

Behaviour the tests pin:
- `Offer` counts consecutive equal states. When the count reaches `Hold` and the state differs from the one applied, it hands the state to the single worker goroutine. The newest state wins; there is never more than one `openrgb` run at a time.
- Before the first change, and again after any failed run, the worker lists devices (`ListArgs`), picks them, and fails if none match.
- A failed list or run sets the note. While the note is set, an `Offer` made `RetryEvery` or more after the failure hands the current held state to the worker again, whether or not it changed.
- The lock is tried in `New`. Without it, `Offer` tries again when `LockEvery` has passed; nothing runs and the note stays "" until it is held.
- After every run the worker calls `PruneLogs(filepath.Join(ConfigDir, "logs"), 5)`.
- `Close` on a controller that never applied a state, or does not hold the lock, runs nothing. `Close` twice is safe.

- [ ] **Step 1: Write the failing tests** in `controller_test.go`, with a fake `Runner` that records each argument list, sends on a channel when called, and returns what the test scripts; a fake clock; `ConfigDir: t.TempDir()`. The fake's default answer to `ListArgs` is the Task 3 `list_detailed.txt` fixture.

| Test | Assertions |
| --- | --- |
| `TestHoldsThreePolls` | `Offer(Green)` twice: no run. Third: one list run, then one run with `-c 00FF00`. |
| `TestFlappingNeverRuns` | 20 alternating `Green`/`Amber` offers: zero runs. |
| `TestChangeAfterHold` | green applied; `Amber` x3: one run with `FF7E00`, no second list run. |
| `TestSameStateNotRepeated` | green applied; 10 more `Green`: no further run. |
| `TestNewestWins` | runner blocked on the green run; `Amber` x3 then `Red` x3; release: the next run is `FF0000`, and `FF7E00` never runs. |
| `TestFailureThenRetry` | runner returns `ErrUnavailable`: `Note() == "led: unavailable"`. Clock +59 s, `Offer(Green)`: no run. Clock +1 s, `Offer(Green)`: list and green run again; on success `Note() == ""`. |
| `TestNoMatchingDevice` | list returns `list_empty.txt`: note set, no colour run. |
| `TestHungRunTimesOut` | runner blocks until its context ends; `Timeout: 20 * time.Millisecond`: `Offer` returns at once every time, the note is set, and `Close` returns within 1 s. |
| `TestNoLockNeverRuns` | `Lock` returns an error: `Red` x3, then `Close`: zero runs, `Note() == ""`. |
| `TestLockTakenLater` | `Lock` fails, then succeeds; clock +30 s; `Red` x3: list and red run. |
| `TestCloseSwitchesOff` | green applied; `Close`: last run ends `-m Off`. A second `Close` adds no run. |
| `TestCloseWithoutChange` | `New` then `Close`: zero runs. |
| `TestNoGoroutineLeft` | after `Close`, the worker has exited (the test waits on a done channel exposed for tests). |

In `prune_test.go`: seven files with increasing modification times in a temp dir, `PruneLogs(dir, 5)` leaves the five newest; a missing dir returns nil; a subdirectory is left alone.

- [ ] **Step 2: Run** `go test ./internal/led/`. Expected: FAIL, `New` undefined.

- [ ] **Step 3: Implement** `controller.go` (state under one mutex; one worker goroutine fed through a one-slot "pending state" guarded by that mutex and a wake channel), `run.go` (`Command`, following `gpu.Query` in `internal/gpu/gpu.go`: `exec.LookPath`, `exec.CommandContext`, `WaitDelay`), `prune.go`.

- [ ] **Step 4: Run** `go test -count=1 ./internal/led/`. Expected: PASS. Run it five times to catch ordering flakes.

- [ ] **Step 5: Commit** `feat: lighting controller with flicker guard, retry and lock`.

---

### Task 5: The model reports the status; the footer shows the note

**Files:**
- Modify: `internal/ui/render.go` (`Options`, `footer`), `internal/ui/model.go` (`pollMsg` handling)
- Test: `internal/ui/model_test.go`, `internal/ui/render_test.go`

**Interfaces:**
- Consumes: `Status`, `Level` (Task 2).
- Produces, in `Options`:

```go
OnStatus func(Level) string // the live model calls it after every poll with the level of that poll; the result is the footer note ("" = none); must not block
LEDNote  string             // drawn in the footer; the model sets it from OnStatus
```

- [ ] **Step 1: Write the failing tests.**
  - Model: with `OnStatus` recording its argument and returning `"led: unavailable"`, a `pollMsg` carrying an unreachable snapshot calls it once with `LevelBad`, and the next `View()` contains `led: unavailable`.
  - Model, paused: after `p`, a `pollMsg` still calls `OnStatus` with the level of the new snapshot (the lights follow live data).
  - Model: `OnStatus == nil` does not panic.
  - Render: with `LEDNote: "led: unavailable"` at width 120 the footer contains the note; in `NoColor` mode it reads `led: unavailable!`; at width 40 the keys are still there, the note is dropped, and the line is exactly 40 cells.
  - Status: `Status(base, Options{LEDNote: "led: unavailable", ...})` is `LevelOK`.

- [ ] **Step 2: Run** `go test ./internal/ui/ -run 'Model|Footer|Status'`. Expected: FAIL.

- [ ] **Step 3: Implement.** In `Update`, on `pollMsg`, compute `Status(msg.snap, o)` with `o.Now = m.now()` and `o.Interval = m.interval` (never the frozen clock), call `OnStatus` when set and store the result in `m.opts.LEDNote`. In `footer`, the note is painted `rWarn` with `warnMark`, passed through `ascii` and capped at 32 cells, and sits to the left of the URL; when the line is too narrow the URL goes first, then the note.

- [ ] **Step 4: Run** `go test ./internal/ui/`. Extend the sweep tests with one case that sets `LEDNote`. Expected: PASS.

- [ ] **Step 5: Commit** `feat: report the status after each poll and show the lighting note`.

---

### Task 6: Settings, wiring, installer and documentation

**Files:**
- Create: `cmd/llamadash/led.go`
- Modify: `cmd/llamadash/main.go`, `cmd/llamadash/config.go`, `internal/collect/power.go`, `contrib/llamadash.conf`, `install.sh`, `README.md`, `docs/ARCHITECTURE.md`, `AGENTS.md`
- Test: `cmd/llamadash/led_test.go`, `cmd/llamadash/main_test.go`, `cmd/llamadash/config_test.go`

**Interfaces:**
- Consumes: `led.New`, `led.Config`, `led.Command`, `led.State` (Tasks 3 and 4); `ui.Level`, `Options.OnStatus` (Tasks 2 and 5); `store.Lock(path)` (existing: flock on `path + ".lock"`); `defaultDBPath(euid, env)` (existing, `cmd/llamadash/db.go`).
- Produces:

```go
// collect: exported form of the existing sourcesAllowed.
func (c *Collector) LocalSources() bool

// cmd/llamadash/led.go
func ledState(l ui.Level) led.State // ok -> Green, warn -> Amber, bad -> Red
// startLED returns nil when the feature is off, the run is one-shot, or local sources are off.
func startLED(o options, c *collect.Collector, env func(string) string, oneShot bool) *led.Controller
```

New option fields: `led bool`, `ledCmd string`, `ledDevice string`.

Settings and flags:

| Setting | Flag | Rule |
| --- | --- | --- |
| `LLAMADASH_LED` | `--led` (bool; default from the setting, so `--led=false` turns it off) | `on`, `off` or empty; anything else: `llamadash: LLAMADASH_LED must be on or off`, exit 2 |
| `LLAMADASH_LED_CMD` | `--led-cmd` | default `openrgb`; at most 4096 bytes, no control characters, else exit 2 |
| `LLAMADASH_LED_DEVICE` | `--led-device` | default empty; at most 64 bytes, no control characters, else exit 2 |

OpenRGB's directory is `filepath.Join(filepath.Dir(<default database path>), "openrgb")`, whatever `--db` or `--no-db` say. The lock is `store.Lock(filepath.Join(<that directory>, "led"))`, taken through `led.Config.Lock`.

- [ ] **Step 1: Write the failing tests.**
  - `parseArgs`: `LLAMADASH_LED=on` sets `o.led`; `LLAMADASH_LED=maybe` exits 2 with the message above; `--led=false` overrides `on`; `LLAMADASH_LED_DEVICE` of 65 bytes exits 2; `LLAMADASH_LED_CMD` containing a newline exits 2; a config file holding the three keys is accepted.
  - `ledState`: the three mappings.
  - `startLED` returns nil for: `o.led == false`; `oneShot == true`; `o.led` with URL `http://192.0.2.1:8080` and no `--gpu`; `o.led` with a loopback URL and `--no-gpu`. It returns a controller for `o.led` with a loopback URL (the test sets `XDG_STATE_HOME` to `t.TempDir()` through `env`, stubs `geteuid` to a non-zero value, and sets `ledCmd` to a path that does not exist; then closes the controller).
  - `collect`: `LocalSources()` is true for a loopback URL, false for `192.0.2.1` in auto mode, true with `GPUOn`, false with `GPUOff`.
  - `run` with `--once` and `LLAMADASH_LED=on` creates no `openrgb` directory under the temp state home.

- [ ] **Step 2: Run** `go test ./cmd/llamadash/ ./internal/collect/`. Expected: FAIL.

- [ ] **Step 3: Implement.** Add the three keys to `configKeys`. Parse and validate in a helper called from `parseArgs`, in the style of `scaleArgs`. In `run`, for the live dashboard only: `ctl := startLED(...)`; when non-nil set `display.OnStatus = func(l ui.Level) string { ctl.Offer(ledState(l)); return ctl.Note() }` and `defer ctl.Close()` so it runs before `finish`.

- [ ] **Step 4: Run** `go test ./...`, `go vet ./...`, `gofmt -l .`. Expected: PASS, clean, empty.

- [ ] **Step 5: Installer and sample settings.** `install.sh` reads the three keys with `conf_get`, as it does `LLAMADASH_KNOWN_FANS`, writes them back into the new settings file under one comment line, and lists them in the summary as kept. It does not ask about them. `contrib/llamadash.conf` gains a commented block:

```
# Show the dashboard's worst colour on the motherboard lighting (needs OpenRGB).
#LLAMADASH_LED=on
# Path of the openrgb program when it is not on PATH.
#LLAMADASH_LED_CMD=/opt/openrgb/openrgb
# Drive this device instead of every motherboard OpenRGB finds.
#LLAMADASH_LED_DEVICE=
```

Check the installer in a container: an existing settings file holding `LLAMADASH_LED=on` and `LLAMADASH_LED_CMD=/opt/openrgb/openrgb` still holds both lines after a re-run.

- [ ] **Step 6: Documentation.**
  - `README.md`: a row in "At a glance"; a "Status lighting" part in "What it shows" (what counts, the three exceptions, the flicker guard, off on exit, lights stay after a crash or `kill -9`, only one copy drives them); the three settings and flags in the reference; Troubleshooting entries for "footer says led: unavailable" (OpenRGB missing or not on root's `PATH`, no motherboard device: run `openrgb --list-detailed`), a non-root dashboard (OpenRGB's udev rules), and boards whose only zones are headers.
  - `docs/ARCHITECTURE.md`: `internal/led` in the package table and the dependency paragraph (`led` depends on nothing in the project; `cmd/llamadash` connects `ui` to it), the status function under "The UI", the three settings under "Settings".
  - `AGENTS.md`: a row "The status lighting | `internal/led`; what counts towards it: `internal/ui/status.go`".

- [ ] **Step 7: Race suite** (Docker): the command in `AGENTS.md`. Expected: PASS.

- [ ] **Step 8: Commit** `feat: drive the motherboard lighting from the dashboard status`.

---

### Task 7: Check on the real machine

Needs the user: the dashboard there runs as root and must be restarted by them.

- [ ] **Step 1:** `python build.py`; copy the amd64 binary to the server next to the running one under a new name.
- [ ] **Step 2:** Put OpenRGB 1.0 for Linux amd64 from openrgb.org into the same home directory and confirm `<path>/openrgb --noautoconnect --list-detailed` lists the board when run as root.
- [ ] **Step 3:** The user adds `LLAMADASH_LED=on` and `LLAMADASH_LED_CMD=<path>` to the settings file and restarts the dashboard with the new binary.
- [ ] **Step 4:** Confirm with the user: lights go green within a few seconds of a healthy screen; stopping the llama-server container turns them red; quitting the dashboard switches them off; a second copy over SSH can be started and quit without the lights changing; after an hour `openrgb/logs` in the state directory holds at most five files.
- [ ] **Step 5:** State plainly in the final report anything from Step 4 that was not observed.
