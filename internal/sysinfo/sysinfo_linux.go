//go:build linux

package sysinfo

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sazda/network-monitor/internal/protocol"
)

// Linux: /proc (agent za servere je namenjen Windows serverima; ovo služi za
// probe i razvoj).
func cpuTimes() (idle, all uint64, err error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, errors.New("neočekivan /proc/stat")
	}
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		all += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return idle, all, nil
}

func memory() (total, used uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		n, _ := strconv.ParseUint(strings.Fields(v + " 0")[0], 10, 64)
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	return total, total - avail, nil
}

func uptime() (uint64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	s, _, _ := strings.Cut(string(b), " ")
	v, err := strconv.ParseFloat(s, 64)
	return uint64(v), err
}

func disks() []protocol.Disk {
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) != nil {
		return nil
	}
	return []protocol.Disk{{Name: "/", Total: st.Blocks * uint64(st.Bsize), Free: st.Bavail * uint64(st.Bsize)}}
}

// processes: /proc/<pid>/stat (utime+stime u istim jedinicama kao /proc/stat,
// rss u stranicama).
func processes() (map[procKey]procSample, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	page := uint64(os.Getpagesize())
	out := map[procKey]procSample{}
	for _, e := range ents {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if i < 0 || j < i {
			continue
		}
		f := strings.Fields(s[j+1:])
		if len(f) < 22 {
			continue
		}
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		rss, _ := strconv.ParseUint(f[21], 10, 64)
		out[procKey{uint32(pid), s[i+1 : j]}] = procSample{cpu: ut + st, mem: rss * page}
	}
	return out, nil
}
