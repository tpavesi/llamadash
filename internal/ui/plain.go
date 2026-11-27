// Package ui renders collector snapshots.
package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/cost"
	"llamadash/internal/llama"
	"llamadash/internal/stats"
	"llamadash/internal/store"
)

// fmtCount formats a token count; values of 10000 or more use k / M with one decimal.
func fmtCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// ascii replaces control and non-ASCII characters in server-provided text.
func ascii(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func fmtElapsed(d time.Duration, approx bool) string {
	s := int(d.Seconds())
	out := fmt.Sprintf("%ds", s)
	if s >= 60 {
		out = fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	if approx {
		out = "~" + out
	}
	return out
}

// fmtEnergy formats watt-hours with about three significant digits; kWh above 1000 Wh.
func fmtEnergy(wh float64) string {
	switch {
	case wh >= 100_000:
		return fmt.Sprintf("%.0f kWh", wh/1000)
	case wh >= 10_000:
		return fmt.Sprintf("%.1f kWh", wh/1000)
	case wh >= 1000:
		return fmt.Sprintf("%.2f kWh", wh/1000)
	case wh >= 100:
		return fmt.Sprintf("%.0f Wh", wh)
	case wh >= 10:
		return fmt.Sprintf("%.1f Wh", wh)
	}
	return fmt.Sprintf("%.2f Wh", wh)
}

// promptCount is the prompt size of a slot: cached plus processed tokens. On
// current builds n_prompt_tokens also counts generated tokens (it equals ctx
// used), so it is only the fallback when the other two are absent.
func promptCount(v stats.SlotView) int {
	if n := v.PromptCache + v.PromptProcessed; n > 0 {
		return n
	}
	return v.PromptTokens
}

func slotLine(v stats.SlotView) string {
	if v.State == stats.Idle { // the counters of an idle slot belong to its last task
		return fmt.Sprintf("slot %d  idle", v.ID)
	}
	rate := v.GenRate
	if v.State == stats.PromptEval {
		rate = v.PromptRate
	}
	return fmt.Sprintf("slot %d  %-11s ctx %s/%s  prompt %s (cached %.0f%%)  generated %s  %.1f tok/s  %s",
		v.ID, v.State, fmtCount(v.CtxUsed), fmtCount(v.NCtx), fmtCount(promptCount(v)), v.CacheShare*100,
		fmtCount(v.Decoded), rate, fmtElapsed(v.Elapsed, v.ElapsedApprox))
}

// plainMoney is cost.Format in plain ASCII ("GBP 1.76").
func plainMoney(v float64, currency string) string {
	return ascii(strings.NewReplacer("£", "GBP ", "€", "EUR ").Replace(cost.Format(v, currency)))
}

// plainLocal writes the host, process and container lines.
func plainLocal(line func(string, ...any), s collect.Snapshot) {
	if h := s.Host; h != nil {
		cpu := "n/a"
		if h.HasCPU {
			cpu = fmt.Sprintf("%.0f%%", h.CPUPct)
		}
		used := h.MemTotalKB - min(h.MemAvailKB, h.MemTotalKB)
		l := fmt.Sprintf("host cpu %s of %d cores  load %.2f %.2f %.2f  ram %s/%s GB  swap %s/%s GB", cpu, h.Cores,
			h.Load1, h.Load5, h.Load15, kbGB(used), kbGB(h.MemTotalKB),
			kbGB(h.SwapTotalKB-min(h.SwapFreeKB, h.SwapTotalKB)), kbGB(h.SwapTotalKB))
		if h.HasDisk {
			l += fmt.Sprintf("  disk / %s free of %s", fmtSize(h.DiskFree), fmtSize(h.DiskTotal))
		}
		if s.HasCPUTemp {
			l += fmt.Sprintf("  temp %.0f C", s.CPUTempC)
		}
		line("%s", l)
	}
	if len(s.Fans) > 0 {
		fans, names := shownFans(s.Fans)
		parts := make([]string, len(fans))
		for i, f := range fans {
			parts[i] = fmt.Sprintf("%s %d", names[i], f.RPM)
			if f.Missing {
				parts[i] = names[i] + " n/a"
			}
			if f.Stopped {
				parts[i] += " STOPPED"
			}
		}
		line("fans  %s rpm", strings.Join(parts, "  "))
	}
	if p := s.Proc; p != nil {
		cpu := "n/a"
		if p.HasCPU {
			cpu = fmt.Sprintf("%.0f%% (of one core)", p.CPUPct)
		}
		line("process pid %d  cpu %s  mem %s GB  threads %d", p.PID, cpu, kbGB(p.RSSKB), p.Threads)
	}
	if c := s.Container; c != nil {
		health := ascii(c.Health)
		if health == "" {
			health = "no healthcheck"
		}
		l := fmt.Sprintf("container %s  %s  %s", ascii(c.Name), ascii(c.State), health)
		if c.State == "running" && !c.StartedAt.IsZero() {
			l += "  up " + fmtUptime(s.At.Sub(c.StartedAt))
		}
		line("%s  restarts %d", l, c.Restarts)
	}
	plainContainers(line, s)
}

// RenderPlain renders s as plain ASCII text without escape sequences.
func RenderPlain(s collect.Snapshot) string {
	var b strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	model, health := "?", "?"
	header := ""
	if p := s.Props; p != nil {
		if p.ModelAlias != "" {
			model = ascii(p.ModelAlias)
		}
		if q := llama.ShortFtype(p.ModelFtype); q != "" {
			model += " " + q
		}
		header = fmt.Sprintf("  ctx %s x %d slots", fmtCount(p.NCtx), p.TotalSlots)
	}
	if s.Health.Status != "" {
		health = ascii(s.Health.Status)
	}
	line("model %s%s  health %s  %s", model, header, health, ascii(s.URL))
	line("status %s", Status(s, Options{Now: s.At, Interval: time.Second}))
	if !s.Reachable {
		since := "unknown"
		if !s.UnreachableSince.IsZero() {
			since = s.UnreachableSince.Format("15:04:05")
		}
		line("server unreachable since %s (data below is stale)", since)
	} else if s.Loading {
		line("server is loading the model")
	}

	for _, v := range s.Slots {
		line("%s", slotLine(v))
	}
	if len(s.Recent) > 0 {
		f := s.Recent[0]
		rate, ttft := "-", "-"
		if f.GenRate > 0 {
			rate = fmtRate(f.GenRate)
		}
		if f.TTFT > 0 {
			ttft = fmtDur(f.TTFT, f.Approx)
		}
		line("recent %d finished, latest %s slot %d  prompt %s (%.0f%% cached)  generated %s  %s  %s tok/s  ttft %s",
			len(s.Recent), fmtWhen(f.At, s.At), f.SlotID, fmtCount(f.PromptTokens), clamp01(f.CacheShare)*100,
			fmtCount(f.Generated), fmtDur(f.Duration, f.Approx), rate, ttft)
	}
	if len(s.Slots) == 0 && s.SlotsHint == "" && s.Reachable {
		line("no slot data")
	}
	if s.SlotsHint != "" {
		line("%s", ascii(s.SlotsHint))
	}
	line("throughput  generation %.1f tok/s  prompt %.1f tok/s", s.GenRate, s.PromptRate)

	if len(s.Metrics) > 0 {
		var parts []string
		for _, m := range []struct{ key, label string }{
			{"requests_processing", "processing"}, {"requests_deferred", "queued"},
			{"prompt_tokens_total", "prompt total"}, {"tokens_predicted_total", "generated total"},
		} {
			if v, ok := s.Metrics[m.key]; ok {
				parts = append(parts, fmt.Sprintf("%s %s", m.label, fmtCount(int(v))))
			}
		}
		if len(parts) > 0 {
			line("metrics  %s", strings.Join(parts, "  "))
		}
	}
	if s.MetricsHint != "" {
		line("%s", ascii(s.MetricsHint))
	}

	if s.HasPower {
		l := fmt.Sprintf("power %.0f W", s.PowerW)
		if s.PSUWatts > 0 {
			l += fmt.Sprintf(" of %s W psu", axisNum(s.PSUWatts))
		}
		if s.HasCPUPower && s.HasGPUPower {
			l += fmt.Sprintf(" (gpu %.0f W, cpu %.0f W)", s.GPUPowerW, s.CPUPowerW)
		}
		if !s.OneShot { // one-shot output covers about a second: no "since start"
			l += fmt.Sprintf("   energy %s since start", fmtEnergy(s.EnergyWh))
		}
		if s.JoulesPerToken > 0 {
			l += fmt.Sprintf("   %.1f J/token", s.JoulesPerToken)
		}
		line("%s", l)
	}
	if c := s.Cost; c.Enabled {
		l := fmt.Sprintf("cost %s since start  %s/day at average draw", plainMoney(c.SinceStart, c.Currency),
			plainMoney(c.PerDay, c.Currency))
		if s.OneShot {
			l = fmt.Sprintf("cost %s/day at current draw", plainMoney(c.PerDay, c.Currency))
		} else if c.PerMTok > 0 {
			l += fmt.Sprintf("  %s per 1M tokens", plainMoney(c.PerMTok, c.Currency))
		}
		var src []string
		if s.HasGPUPower {
			src = append(src, "gpu")
		}
		if s.HasCPUPower {
			src = append(src, "cpu")
		}
		basis := tariff(c, func(v float64) string { return plainMoney(v, c.Currency) })
		if len(src) > 0 {
			basis = strings.Join(src, "+") + ", " + basis
		}
		line("%s  (%s)", l, basis)
	}
	for _, row := range []struct {
		name string
		t    store.Totals
	}{{"today", s.Today}, {"month", s.Month}, {"all time", s.AllTime}} {
		if !row.t.HasData {
			continue
		}
		u := row.t.Usage
		l := fmt.Sprintf("%s %s", row.name, fmtEnergy(u.GPUEnergyWh+u.CPUEnergyWh))
		if s.Cost.Enabled {
			l += "  " + plainMoney(u.Cost, s.Cost.Currency)
		}
		line("%s  %s gen tokens  %s requests", l, fmtCount(int(u.GenTokens)), fmtCount(int(u.Requests)))
	}
	if s.DBStatus != "" {
		line("db: %s", ascii(s.DBStatus))
	}
	plainLocal(line, s)
	gpus := append(s.GPUs[:0:0], s.GPUs...)
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	for _, g := range gpus {
		l := fmt.Sprintf("GPU %d (this host)  %s", g.Index, ascii(g.Name))
		if g.HasUtil {
			l += fmt.Sprintf("  util %.0f%%", g.UtilPct)
		}
		l += fmt.Sprintf("  mem %.0f/%.0f MiB", g.MemUsedMiB, g.MemTotalMiB)
		if g.HasTemp {
			l += fmt.Sprintf("  %.0f C", g.TempC)
		}
		line("%s", l)
	}
	return b.String()
}
