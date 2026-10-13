// Package sysinfo reads host resource figures for the status page (Linux;
// on a Raspberry Pi this includes the SoC temperature). Every value is
// best effort: missing sources leave fields nil instead of failing.
package sysinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Info struct {
	Load1      *float64 `json:"load1"`
	Load5      *float64 `json:"load5"`
	Load15     *float64 `json:"load15"`
	CPUs       int      `json:"cpus"`
	MemTotal   *uint64  `json:"mem_total"` // bytes
	MemAvail   *uint64  `json:"mem_available"`
	TempC      *float64 `json:"temp_c"`
	DiskTotal  *uint64  `json:"disk_total"` // of the data directory's filesystem
	DiskFree   *uint64  `json:"disk_free"`
	UptimeSecs *int64   `json:"uptime_seconds"`
}

// Read collects host figures; dataDir selects the filesystem for disk usage.
func Read(dataDir string) Info {
	var in Info
	in.CPUs = cpus()
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			in.Load1, in.Load5, in.Load15 = pf(f[0]), pf(f[1]), pf(f[2])
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			if err != nil {
				continue
			}
			b := kb * 1024
			switch k {
			case "MemTotal":
				in.MemTotal = &b
			case "MemAvailable":
				in.MemAvail = &b
			}
		}
		f.Close()
	}
	// thermal_zone0 is the SoC on a Raspberry Pi; on PCs it may be any sensor.
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			t := milli / 1000
			in.TempC = &t
		}
	}
	var st syscall.Statfs_t
	if dataDir != "" && syscall.Statfs(dataDir, &st) == nil {
		total, free := st.Blocks*uint64(st.Bsize), st.Bavail*uint64(st.Bsize)
		in.DiskTotal, in.DiskFree = &total, &free
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
				s := int64(time.Duration(secs * float64(time.Second)).Seconds())
				in.UptimeSecs = &s
			}
		}
	}
	return in
}

func pf(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

func cpus() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}
