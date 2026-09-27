package metrics

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/dokwalt/dokwalt/internal/api"
)

// hostCPU holds the previous /proc/stat totals to compute utilization.
type hostCPU struct {
	idle, total uint64
}

func readProcStatCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle + iowait
			idle += n
		}
	}
	return idle, total, true
}

func (h *hostCPU) sample() float64 {
	idle, total, ok := readProcStatCPU()
	if !ok {
		return 0
	}
	defer func() { h.idle, h.total = idle, total }()
	if h.total == 0 || total <= h.total {
		return 0
	}
	dt := float64(total - h.total)
	di := float64(idle - h.idle)
	return (1 - di/dt) * 100
}

func readMeminfo() (used, total uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	if total > avail {
		used = total - avail
	}
	return used, total
}

func diskUsage(path string) (used, total uint64) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0
	}
	total = s.Blocks * uint64(s.Bsize)
	free := s.Bavail * uint64(s.Bsize)
	return total - free, total
}

func load1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// temperature returns the SoC/CPU temperature in °C when available.
func temperature() float64 {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v / 1000
}

// throttled decodes `vcgencmd get_throttled` on a Raspberry Pi.
func throttled() string {
	path, err := exec.LookPath("vcgencmd")
	if err != nil {
		return ""
	}
	out, err := exec.Command(path, "get_throttled").Output()
	if err != nil {
		return ""
	}
	_, hex, _ := strings.Cut(strings.TrimSpace(string(out)), "=")
	v, err := strconv.ParseUint(strings.TrimPrefix(hex, "0x"), 16, 32)
	if err != nil || v == 0 {
		return ""
	}
	var now, past []string
	flags := []struct {
		bit  uint
		name string
	}{{0, "under-voltage"}, {1, "frequency capped"}, {2, "throttled"}, {3, "soft temperature limit"}}
	for _, f := range flags {
		if v&(1<<f.bit) != 0 {
			now = append(now, f.name)
		}
		if v&(1<<(f.bit+16)) != 0 {
			past = append(past, f.name)
		}
	}
	switch {
	case len(now) > 0:
		return "now: " + strings.Join(now, ", ")
	case len(past) > 0:
		return "since boot: " + strings.Join(past, ", ")
	}
	return ""
}

// ProcessRSS returns the resident memory of a process (0 if unknown).
func ProcessRSS(pid int) uint64 {
	rss, _ := ProcessMem(pid)
	return rss
}

// ProcessMem returns resident memory and its anonymous part (heap, stacks):
// the rest is code mapped from the binary, shared and reclaimable.
func ProcessMem(pid int) (rss, anon uint64) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "VmRSS:":
			rss = n * 1024
		case "RssAnon:":
			anon = n * 1024
		}
	}
	return rss, anon
}

func hostMetrics(cpu *hostCPU, diskPath string, throttle string) api.HostMetrics {
	used, total := readMeminfo()
	du, dt := diskUsage(diskPath)
	return api.HostMetrics{
		CPUPct: cpu.sample(), CPUs: runtime.NumCPU(),
		MemUsed: used, MemTotal: total, DiskUsed: du, DiskTotal: dt,
		Load1: load1(), TempC: temperature(), Throttled: throttle,
	}
}
