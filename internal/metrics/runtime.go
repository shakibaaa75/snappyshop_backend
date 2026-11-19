package metrics

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Runtime describes the Go process and, on Linux, the machine it runs on.
type Runtime struct {
	GoVersion     string  `json:"goVersion"`
	OS            string  `json:"os"`
	Arch          string  `json:"arch"`
	CPUs          int     `json:"cpus"`
	GoMaxProcs    int     `json:"goMaxProcs"`
	Goroutines    int     `json:"goroutines"`
	HeapAllocMB   float64 `json:"heapAllocMb"` // live heap
	HeapSysMB     float64 `json:"heapSysMb"`   // heap memory obtained from the OS
	StackMB       float64 `json:"stackMb"`
	SysMB         float64 `json:"sysMb"` // total memory obtained from the OS
	GCCycles      uint32  `json:"gcCycles"`
	GCPauseTotal  float64 `json:"gcPauseTotalMs"`
	LastGCSecAgo  float64 `json:"lastGcSecondsAgo"`
	UptimeSeconds int64   `json:"uptimeSeconds"`

	// Host values come from /proc and are only present on Linux.
	Host *Host `json:"host,omitempty"`
}

// Host is machine-level load and memory.
type Host struct {
	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	Load15        float64 `json:"load15"`
	MemTotalMB    float64 `json:"memTotalMb"`
	MemAvailMB    float64 `json:"memAvailableMb"`
	MemUsedPct    float64 `json:"memUsedPercent"`
	SwapUsedPct   float64 `json:"swapUsedPercent"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

const mb = 1024 * 1024

// ReadRuntime reads the process runtime numbers; started is the process start.
func ReadRuntime(started time.Time) Runtime {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	rt := Runtime{
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUs:          runtime.NumCPU(),
		GoMaxProcs:    runtime.GOMAXPROCS(0),
		Goroutines:    runtime.NumGoroutine(),
		HeapAllocMB:   float64(m.HeapAlloc) / mb,
		HeapSysMB:     float64(m.HeapSys) / mb,
		StackMB:       float64(m.StackInuse) / mb,
		SysMB:         float64(m.Sys) / mb,
		GCCycles:      m.NumGC,
		GCPauseTotal:  float64(m.PauseTotalNs) / 1e6,
		UptimeSeconds: int64(time.Since(started).Seconds()),
	}
	if m.LastGC > 0 {
		rt.LastGCSecAgo = time.Since(time.Unix(0, int64(m.LastGC))).Seconds()
	}
	rt.Host = readHost()
	return rt
}

// Started returns when the collector was created (process start).
func (c *Collector) Started() time.Time { return c.start }

func readHost() *Host {
	h := &Host{}
	got := false

	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			h.Load1, _ = strconv.ParseFloat(f[0], 64)
			h.Load5, _ = strconv.ParseFloat(f[1], 64)
			h.Load15, _ = strconv.ParseFloat(f[2], 64)
			got = true
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 1 {
			h.UptimeSeconds, _ = strconv.ParseFloat(f[0], 64)
			got = true
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		kb := map[string]float64{}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				kb[strings.TrimSuffix(f[0], ":")] = v
			}
		}
		if total := kb["MemTotal"]; total > 0 {
			avail, ok := kb["MemAvailable"]
			if !ok {
				avail = kb["MemFree"]
			}
			h.MemTotalMB = total / 1024
			h.MemAvailMB = avail / 1024
			h.MemUsedPct = (total - avail) * 100 / total
			if st := kb["SwapTotal"]; st > 0 {
				h.SwapUsedPct = (st - kb["SwapFree"]) * 100 / st
			}
			got = true
		}
	}
	if !got {
		return nil
	}
	return h
}
