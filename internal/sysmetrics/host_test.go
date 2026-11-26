package sysmetrics

import (
	"strings"
	"testing"
)

func TestParsingTheKernelFiles(t *testing.T) {
	var h Host
	if !parseLoad("1.27 1.11 1.04 2/301 4021\n", &h) || h.Load1 != 1.27 || h.Load5 != 1.11 || h.Load15 != 1.04 {
		t.Fatalf("load = %+v", h)
	}
	if parseLoad("nonsense", &Host{}) || parseLoad("a b c", &Host{}) {
		t.Fatal("bad input must be refused")
	}
	parseMeminfo(strings.NewReader("MemTotal:        2000 kB\nMemFree:   10 kB\nMemAvailable:     500 kB\nSwapTotal:  100 kB\nSwapFree:    40 kB\nbroken\n"), &h)
	if h.MemTotal != 2000*1024 || h.MemAvail != 500*1024 || h.SwapTotal != 100*1024 || h.SwapFree != 40*1024 {
		t.Fatalf("memory = %+v", h)
	}
	if h.MemUsedPercent() != 75 {
		t.Fatalf("memory used = %v", h.MemUsedPercent())
	}
	h.DiskUsed, h.DiskFree = 39, 7
	if h.DiskUsedPercent() != 84.8 || (Host{}).DiskUsedPercent() != 0 {
		t.Fatalf("disk used = %v", h.DiskUsedPercent())
	}
}

func TestReadingThisMachine(t *testing.T) {
	h, ok := Read()
	if !ok {
		t.Skip("this system cannot be measured")
	}
	if h.CPUCount < 1 || h.MemTotal <= 0 || h.MemAvail <= 0 || h.MemAvail > h.MemTotal || h.DiskTotal <= 0 || h.DiskFree <= 0 || h.DiskUsed <= 0 || h.UptimeSeconds <= 0 {
		t.Fatalf("implausible reading: %+v", h)
	}
}
