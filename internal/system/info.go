package system

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Status is the snapshot the agent reports to the site on each heartbeat.
type Status struct {
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Kernel       string    `json:"kernel"`
	Uptime       int64     `json:"uptimeSeconds"`
	CPUCores     int       `json:"cpuCores"`
	LoadAvg1     float64   `json:"loadAvg1"`
	LoadAvg5     float64   `json:"loadAvg5"`
	LoadAvg15    float64   `json:"loadAvg15"`
	MemTotalKB   uint64    `json:"memTotalKb"`
	MemAvailKB   uint64    `json:"memAvailableKb"`
	MemUsedKB    uint64    `json:"memUsedKb"`
	DiskTotalKB  uint64    `json:"diskTotalKb"`
	DiskFreeKB   uint64    `json:"diskFreeKb"`
	DiskUsedKB   uint64    `json:"diskUsedKb"`
	AgentVersion string    `json:"agentVersion"`
	CollectedAt  time.Time `json:"collectedAt"`
}

// Collect gathers a host status snapshot. It reads from /proc and statfs and is
// best-effort: a failure on one field leaves that field at its zero value
// rather than failing the whole collection (we still want a heartbeat).
func Collect(agentVersion string) Status {
	s := Status{
		AgentVersion: agentVersion,
		OS:           runtime.GOOS,
		CPUCores:     runtime.NumCPU(),
		CollectedAt:  time.Now().UTC(),
	}
	if h, err := os.Hostname(); err == nil {
		s.Hostname = h
	}
	s.Kernel = readKernel()
	s.Uptime = readUptime()
	s.LoadAvg1, s.LoadAvg5, s.LoadAvg15 = readLoadAvg()
	s.MemTotalKB, s.MemAvailKB = readMem()
	if s.MemTotalKB >= s.MemAvailKB {
		s.MemUsedKB = s.MemTotalKB - s.MemAvailKB
	}
	s.DiskTotalKB, s.DiskFreeKB = readDisk("/")
	if s.DiskTotalKB >= s.DiskFreeKB {
		s.DiskUsedKB = s.DiskTotalKB - s.DiskFreeKB
	}
	return s
}

func readKernel() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func readUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	f, _ := strconv.ParseFloat(fields[0], 64)
	return int64(f)
}

func readLoadAvg() (l1, l5, l15 float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return
	}
	l1, _ = strconv.ParseFloat(fields[0], 64)
	l5, _ = strconv.ParseFloat(fields[1], 64)
	l15, _ = strconv.ParseFloat(fields[2], 64)
	return
}

// readMem returns total and available memory in KB by parsing /proc/meminfo.
func readMem() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMeminfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseMeminfoKB(line)
		}
	}
	return
}

func parseMeminfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// readDisk returns total and free space in KB for the filesystem at path. Its
// implementation is OS-specific (see disk_linux.go / disk_other.go).
