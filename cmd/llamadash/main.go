// Command llamadash is a terminal dashboard for a llama.cpp llama-server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"llamadash/internal/collect"
	"llamadash/internal/cost"
	"llamadash/internal/ui"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const (
	minInterval = 250 * time.Millisecond
	maxInterval = 10 * time.Second
	historyWait = 2 * time.Second // bound on the --frame history read
)

type options struct {
	url, apiKey string
	interval    time.Duration
	gpu         collect.GPUMode
	ascii       bool
	once        bool
	version     bool
	frameW      int // --frame COLSxROWS; 0 when not given
	frameH      int
	kwhPrice    float64 // 0 = cost off
	currency    string  // upper case; "" or three letters
	db          string  // "" = the default path
	noDB        bool
	retainDays  int
	rng         ui.Range // the charts' starting range
	psuWatts    float64  // the power supply's maximum wattage; 0 = not set
	axisPower   float64  // learned chart tops from the settings; 0 = not set
	axisGen     float64
	axisPrompt  float64
	confFile    string         // the settings file learned tops are written to; "" = none
	knownFans   []string       // fans seen spinning in earlier runs (LLAMADASH_KNOWN_FANS)
	onNewFans   func([]string) // the live dashboard's saver; nil: not saved
	led         bool           // show the status on the motherboard lighting
	ledCmd      string         // the openrgb program
	ledDevice   string         // "" = every motherboard openrgb finds
}

// scales are the chart tops to start from: the power chart goes to the larger of
// the power supply's wattage and the learned power top.
func (o options) scales() ui.Scales {
	return ui.Scales{PowerW: max(o.psuWatts, o.axisPower), GenTPS: o.axisGen, PromptTPS: o.axisPrompt}
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// parseArgs returns the options and, when the program should exit now, its exit
// code (otherwise -1).
func parseArgs(args []string, env func(string) string, stderr io.Writer) (options, error, int) {
	env, confFile, cfgErr := withConfig(args, env)
	if cfgErr != nil {
		fmt.Fprintln(stderr, "llamadash: config file", cfgErr)
		return options{}, cfgErr, 2
	}
	defURL := "http://localhost:8080"
	if v := env("LLAMADASH_URL"); v != "" {
		defURL = v
	}
	var o options
	var useGPU, noGPU bool
	fs := flag.NewFlagSet("llamadash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.url, "url", defURL, "llama-server base URL (env LLAMADASH_URL)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key sent as a Bearer token (env LLAMA_API_KEY)")
	fs.DurationVar(&o.interval, "interval", time.Second, "poll interval (250ms to 10s)")
	fs.BoolVar(&useGPU, "gpu", false, "read the local GPU, CPU power, host, llama-server process and container readings even for a non-loopback URL")
	fs.BoolVar(&noGPU, "no-gpu", false, "never read the local GPU, CPU power, host, llama-server process or container readings")
	fs.BoolVar(&o.ascii, "ascii", false, "ASCII-only output")
	fs.BoolVar(&o.once, "once", false, "print one plain-text snapshot and exit (for people, not a stable format)")
	fs.String("config", "", "settings file (env LLAMADASH_CONFIG; default /etc/llamadash.conf, then ~/.config/llamadash/llamadash.conf)")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")
	frame := fs.String("frame", "", "print one dashboard frame of COLSxROWS (e.g. 135x120) and exit")
	price := fs.String("kwh-price", env("LLAMADASH_KWH_PRICE"), "energy price per kWh, e.g. 0.3031; no cost figures without it (env LLAMADASH_KWH_PRICE)")
	fs.StringVar(&o.currency, "currency", env("LLAMADASH_CURRENCY"), "three-letter currency code of the price, e.g. GBP (env LLAMADASH_CURRENCY)")
	fs.StringVar(&o.db, "db", "", "history database (env LLAMADASH_DB; default /var/lib/llamadash/llamadash.db as root, else $XDG_STATE_HOME/llamadash or ~/.local/state/llamadash)")
	fs.BoolVar(&o.noDB, "no-db", false, "keep no history database")
	fs.IntVar(&o.retainDays, "retain-days", 30, "days of samples to keep in the database, 1 to 3650 (finished requests are kept 90 days)")
	rng := fs.String("range", "5m", "starting chart range: 5m (live), 1h, 24h or 7d from the database; the h key cycles it")
	psu := fs.String("psu-watts", env("LLAMADASH_PSU_WATTS"), "maximum wattage of the power supply, e.g. 1000: the top of the power chart (env LLAMADASH_PSU_WATTS)")
	ledOn, _ := ledDefault(env) // a bad value is reported by ledArgs
	ledCmd := "openrgb"
	if v := env("LLAMADASH_LED_CMD"); strings.TrimSpace(v) != "" {
		ledCmd = v
	}
	fs.BoolVar(&o.led, "led", ledOn, "show the dashboard's worst colour on the motherboard lighting, through OpenRGB (env LLAMADASH_LED=on)")
	fs.StringVar(&o.ledCmd, "led-cmd", ledCmd, "the openrgb program, a name on PATH or a path (env LLAMADASH_LED_CMD)")
	fs.StringVar(&o.ledDevice, "led-device", env("LLAMADASH_LED_DEVICE"), "drive the OpenRGB device with this name instead of every motherboard (env LLAMADASH_LED_DEVICE)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, err, 0
		}
		return o, err, 2
	}
	if code, err := dbAndCostArgs(&o, fs, *price, env, stderr); code >= 0 {
		return o, err, code
	}
	if code, err := scaleArgs(&o, *psu, env, stderr); code >= 0 {
		return o, err, code
	}
	if code, err := ledArgs(&o, env, stderr); code >= 0 {
		return o, err, code
	}
	o.confFile = confFile
	r, err := ui.ParseRange(*rng)
	if err != nil {
		fmt.Fprintln(stderr, "llamadash: --range must be 5m, 1h, 24h or 7d")
		return o, err, 2
	}
	o.rng = r
	if !validURL(o.url) {
		fmt.Fprintln(stderr, "llamadash: --url must look like http://host:port")
		return o, errors.New("bad url"), 2
	}
	if o.apiKey == "" {
		o.apiKey = env("LLAMA_API_KEY") // applied after parsing so usage output never echoes it
	}
	if *frame != "" {
		w, h, ok := parseSize(*frame)
		if !ok {
			fmt.Fprintf(stderr, "llamadash: --frame must be COLSxROWS, for example 135x120\n")
			return o, errors.New("bad frame"), 2
		}
		o.frameW, o.frameH = w, h
		if o.once {
			fmt.Fprintln(stderr, "llamadash: --frame and --once are mutually exclusive")
			return o, errors.New("conflicting output flags"), 2
		}
	}
	if useGPU && noGPU {
		fmt.Fprintln(stderr, "llamadash: --gpu and --no-gpu are mutually exclusive")
		return o, errors.New("conflicting gpu flags"), 2
	}
	if o.interval < minInterval || o.interval > maxInterval {
		fmt.Fprintf(stderr, "llamadash: --interval must be between %v and %v\n", minInterval, maxInterval)
		return o, errors.New("bad interval"), 2
	}
	switch {
	case useGPU:
		o.gpu = collect.GPUOn
	case noGPU:
		o.gpu = collect.GPUOff
	}
	return o, nil, -1
}

// dbAndCostArgs checks and completes the price, currency and database options; it
// returns -1 when they are fine, else the exit code.
func dbAndCostArgs(o *options, fs *flag.FlagSet, price string, env func(string) string, stderr io.Writer) (int, error) {
	if price != "" {
		p, err := cost.ParsePrice(price)
		if err != nil {
			fmt.Fprintln(stderr, "llamadash: --kwh-price:", err)
			return 2, err
		}
		o.kwhPrice = p
	}
	o.currency = strings.ToUpper(o.currency)
	if !validCurrency(o.currency) {
		fmt.Fprintln(stderr, "llamadash: --currency must be a three-letter code such as GBP")
		return 2, errors.New("bad currency")
	}
	dbSet := false
	fs.Visit(func(f *flag.Flag) { dbSet = dbSet || f.Name == "db" })
	if dbSet && o.noDB {
		fmt.Fprintln(stderr, "llamadash: --db and --no-db are mutually exclusive")
		return 2, errors.New("conflicting db flags")
	}
	if !dbSet {
		o.db = env("LLAMADASH_DB")
	}
	if o.retainDays < 1 || o.retainDays > 3650 {
		fmt.Fprintln(stderr, "llamadash: --retain-days must be between 1 and 3650")
		return 2, errors.New("bad retain-days")
	}
	return -1, nil
}

// validAxis reports whether v may be a learned chart top: above 0, at most 1e9.
func validAxis(v float64) bool { return v > 0 && v <= 1e9 }

// scaleArgs checks the power supply wattage (1 to 100000; a person sets it, so a
// bad one is an error) and reads the learned chart tops. The program writes those,
// so a bad one is not fatal: it is ignored with a warning (the chart learns its
// top again and the next save replaces the line). It returns -1 when the
// settings are usable, else the exit code.
func scaleArgs(o *options, psu string, env func(string) string, stderr io.Writer) (int, error) {
	if psu != "" {
		f, err := strconv.ParseFloat(strings.TrimSpace(psu), 64)
		if err != nil || !(f >= 1 && f <= 100000) {
			fmt.Fprintln(stderr, "llamadash: --psu-watts (LLAMADASH_PSU_WATTS) must be a wattage from 1 to 100000")
			return 2, errors.New("bad psu-watts")
		}
		o.psuWatts = f
	}
	for _, a := range []struct {
		key string
		dst *float64
	}{{"LLAMADASH_AXIS_POWER_W", &o.axisPower}, {"LLAMADASH_AXIS_GEN_TPS", &o.axisGen}, {"LLAMADASH_AXIS_PROMPT_TPS", &o.axisPrompt}} {
		v := env(a.key)
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || !validAxis(f) {
			fmt.Fprintf(stderr, "llamadash: ignoring %s: not a number above 0 and at most 1000000000; the chart learns its scale again\n", a.key)
			continue
		}
		*a.dst = f
	}
	if v := env("LLAMADASH_KNOWN_FANS"); v != "" {
		var bad int
		if o.knownFans, bad = parseKnownFans(v); bad > 0 {
			fmt.Fprintf(stderr, "llamadash: ignoring %d malformed LLAMADASH_KNOWN_FANS entries (each is chip:fanN)\n", bad)
		}
	}
	return -1, nil
}

// validCurrency accepts "" or three ASCII letters.
func validCurrency(c string) bool {
	if c == "" {
		return true
	}
	if len(c) != 3 {
		return false
	}
	for i := range len(c) {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// validURL reports whether raw is an http or https URL with a host.
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

// parseSize parses "COLSxROWS" with both between 1 and 10000.
func parseSize(v string) (w, h int, ok bool) {
	a, b, found := strings.Cut(strings.ToLower(v), "x")
	if !found {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(a)
	h, errH := strconv.Atoi(b)
	if errW != nil || errH != nil || w < 1 || h < 1 || w > 10000 || h > 10000 {
		return 0, 0, false
	}
	return w, h, true
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// twoPolls polls twice one interval apart, so rates have a baseline. The GPU
// query started by the first poll runs in the background: wait (up to its own
// 2s timeout) for it, so the second poll shows its result.
func twoPolls(c *collect.Collector, interval time.Duration) collect.Snapshot {
	ctx := context.Background()
	c.Poll(ctx, time.Now())
	time.Sleep(interval)
	c.WaitGPU(2 * time.Second)
	return c.Poll(ctx, time.Now())
}

// quitOnSignal calls quit for every signal on sigs until done is closed.
func quitOnSignal(sigs <-chan os.Signal, quit func(), done <-chan struct{}) {
	for {
		select {
		case <-sigs:
			quit()
		case <-done:
			return
		}
	}
}

func run(args []string, env func(string) string, stdout, stderr io.Writer) int {
	o, _, code := parseArgs(args, env, stderr)
	if code >= 0 {
		return code
	}
	if o.version {
		fmt.Fprintln(stdout, "llamadash", version)
		return 0
	}
	if !o.once && o.frameW == 0 && !isTerminal(stdout) {
		fmt.Fprintln(stderr, "not a terminal: use --once")
		return 2
	}
	oneShot := o.once || o.frameW > 0 // one-shot output records nothing
	var saver *scaleSaver             // learned chart tops and known fans go to the settings file
	if !oneShot {
		if saver = scaleSink(o); saver != nil {
			o.onNewFans = saver.OfferFans
		}
	}
	c, src, finish := startSession(o, env, oneShot)
	defer finish()
	display := ui.DetectOptions(env)
	display.ASCII = display.ASCII || o.ascii
	display.Range = o.rng
	display.Scales = o.scales()

	if !oneShot {
		if saver != nil {
			display.OnScales = saver.Offer
			defer saver.Close() // before finish: writes what is pending
		}
		if ctl := startLED(o, c, env, oneShot); ctl != nil {
			display.OnStatus = func(l ui.Level) string { ctl.Offer(ledState(l)); return ctl.Note() }
			defer ctl.Close() // before finish: switches the lights off
		}
		p := tea.NewProgram(ui.NewModel(c, o.interval, display, src), tea.WithOutput(stdout))
		// Bubble Tea handles SIGINT and SIGTERM; a hangup (a dropped SSH session) must
		// also take the normal exit path, which flushes and closes the database.
		// (Windows never delivers SIGHUP: there this only waits for done.)
		hup, done := make(chan os.Signal, 1), make(chan struct{})
		signal.Notify(hup, syscall.SIGHUP)
		go quitOnSignal(hup, p.Quit, done)
		_, err := p.Run()
		signal.Stop(hup)
		close(done)
		if err != nil {
			fmt.Fprintln(stderr, "llamadash:", err)
			return 1
		}
		return 0
	}

	s := twoPolls(c, o.interval)
	if o.once {
		fmt.Fprint(stdout, ui.RenderPlain(s))
	} else {
		display.Width, display.Height = o.frameW, o.frameH
		display.Interval, display.Now = o.interval, s.At
		display.History = src != nil
		display = ui.LoadHistory(src, s, display, historyWait)
		fmt.Fprintln(stdout, ui.Render(s, display))
	}
	if !s.Reachable {
		return 1
	}
	return 0
}
