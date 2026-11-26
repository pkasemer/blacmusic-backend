// Package sysmetrics measures the server: processor load, memory, disk and what the app stores.
//
// Inside a container /proc/loadavg, /proc/meminfo and /proc/uptime describe the whole machine, and the container's
// root file system sits on the machine's disk, so no special access to the host is needed.
package sysmetrics

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Host is one reading of the machine.
type Host struct {
	CPUCount             int
	Load1, Load5, Load15 float64
	MemTotal, MemAvail   int64
	SwapTotal, SwapFree  int64
	UptimeSeconds        int64
	// Disk of the root file system. DiskUsed + DiskFree is a little less than DiskTotal: the system keeps a reserve.
	DiskTotal, DiskUsed, DiskFree int64
}

// DiskUsedPercent is the share of the usable disk that is taken, as df shows it.
func (h Host) DiskUsedPercent() float64 { return percent(h.DiskUsed, h.DiskUsed+h.DiskFree) }

// MemUsedPercent is the share of memory that is not available to new programs.
func (h Host) MemUsedPercent() float64 { return percent(h.MemTotal-h.MemAvail, h.MemTotal) }

func percent(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(int64(float64(part)/float64(whole)*1000+0.5)) / 10
}

// Read measures the machine. ok is false where this cannot be done (not Linux).
func Read() (Host, bool) {
	h := Host{CPUCount: runtime.NumCPU()}
	load, err := os.ReadFile("/proc/loadavg")
	if err != nil || !parseLoad(string(load), &h) {
		return Host{}, false
	}
	mem, err := os.Open("/proc/meminfo")
	if err != nil {
		return Host{}, false
	}
	defer mem.Close()
	parseMeminfo(mem, &h)
	if up, err := os.ReadFile("/proc/uptime"); err == nil {
		if first, _, ok := strings.Cut(string(up), " "); ok {
			secs, _ := strconv.ParseFloat(first, 64)
			h.UptimeSeconds = int64(secs)
		}
	}
	var ok bool
	if h.DiskTotal, h.DiskUsed, h.DiskFree, ok = Disk(); !ok || h.MemTotal == 0 {
		return Host{}, false
	}
	return h, true
}

func parseLoad(s string, h *Host) bool {
	f := strings.Fields(s)
	if len(f) < 3 {
		return false
	}
	var errs [3]error
	h.Load1, errs[0] = strconv.ParseFloat(f[0], 64)
	h.Load5, errs[1] = strconv.ParseFloat(f[1], 64)
	h.Load15, errs[2] = strconv.ParseFloat(f[2], 64)
	return errs[0] == nil && errs[1] == nil && errs[2] == nil
}

// parseMeminfo reads "MemTotal:  2014976 kB" lines.
func parseMeminfo(r io.Reader, h *Host) {
	into := map[string]*int64{"MemTotal:": &h.MemTotal, "MemAvailable:": &h.MemAvail, "SwapTotal:": &h.SwapTotal, "SwapFree:": &h.SwapFree}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		if dst := into[f[0]]; dst != nil {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			*dst = kb * 1024
		}
	}
}
