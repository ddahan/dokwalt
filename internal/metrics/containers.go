package metrics

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/dokwalt/dokwalt/internal/docker"
)

// rawStats are cumulative counters for one container.
type rawStats struct {
	cpuUsec    uint64
	memBytes   uint64
	memLimit   uint64
	netRx      uint64
	netTx      uint64
	blkR, blkW uint64
}

// cgroupDir finds a container's cgroup v2 directory.
func cgroupDir(id string) string {
	for _, p := range []string{
		"/sys/fs/cgroup/system.slice/docker-" + id + ".scope",
		"/sys/fs/cgroup/docker/" + id,
		"/sys/fs/cgroup/docker.slice/docker-" + id + ".scope",
	} {
		if _, err := os.Stat(p + "/cpu.stat"); err == nil {
			return p
		}
	}
	return ""
}

func readKV(path string) map[string]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok {
			n, _ := strconv.ParseUint(v, 10, 64)
			out[k] = n
		}
	}
	return out
}

func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// readCgroup reads counters straight from cgroup v2 files: far cheaper than
// the Docker stats API, which matters on a Raspberry Pi.
func readCgroup(dir string, pid int) (rawStats, bool) {
	var s rawStats
	cpu := readKV(dir + "/cpu.stat")
	if cpu == nil {
		return s, false
	}
	s.cpuUsec = cpu["usage_usec"]
	s.memBytes = readUint(dir + "/memory.current")
	if ms := readKV(dir + "/memory.stat"); ms != nil && ms["inactive_file"] < s.memBytes {
		s.memBytes -= ms["inactive_file"] // same "working set" as `docker stats`
	}
	if b, err := os.ReadFile(dir + "/memory.max"); err == nil && strings.TrimSpace(string(b)) != "max" {
		s.memLimit, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	}
	if b, err := os.ReadFile(dir + "/io.stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			for _, f := range strings.Fields(line) {
				k, v, _ := strings.Cut(f, "=")
				n, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "rbytes":
					s.blkR += n
				case "wbytes":
					s.blkW += n
				}
			}
		}
	}
	if pid > 0 {
		s.netRx, s.netTx = netDev(pid)
	}
	return s, true
}

// netDev sums interface counters inside the container's network namespace.
func netDev(pid int) (rx, tx uint64) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// statsFromAPI is the fallback when cgroup files are not readable.
func statsFromAPI(ctx context.Context, dc *docker.Client, id string) (rawStats, bool) {
	st, err := dc.StatsOnce(ctx, id)
	if err != nil {
		return rawStats{}, false
	}
	s := rawStats{
		cpuUsec:  st.CPUStats.CPUUsage.TotalUsage / 1000,
		memBytes: st.MemoryStats.Usage,
		memLimit: st.MemoryStats.Limit,
	}
	if inactive := st.MemoryStats.Stats["inactive_file"]; inactive < s.memBytes {
		s.memBytes -= inactive
	}
	for _, n := range st.Networks {
		s.netRx += n.RxBytes
		s.netTx += n.TxBytes
	}
	for _, b := range st.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			s.blkR += b.Value
		case "write":
			s.blkW += b.Value
		}
	}
	return s, true
}
