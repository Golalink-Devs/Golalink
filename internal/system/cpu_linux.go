package system

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func cpuTimes() (float64, float64, float64, error) {
	var r syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_SELF, &r); e != nil {
		return 0, 0, 0, e
	}
	process := float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
	b, e := os.ReadFile("/proc/stat")
	if e != nil {
		return 0, 0, 0, e
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return 0, 0, 0, fmt.Errorf("invalid /proc/stat")
	}
	var total, idle float64
	for n, f := range fields[1:] {
		if n >= 8 {
			break
		}
		v, e := strconv.ParseFloat(f, 64)
		if e != nil {
			return 0, 0, 0, e
		}
		total += v
		if n == 3 || n == 4 {
			idle += v
		}
	}
	return process, total, idle, nil
}
