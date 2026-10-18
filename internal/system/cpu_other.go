//go:build !windows && !linux

package system

import "errors"

func cpuTimes() (float64, float64, float64, error) {
	return 0, 0, 0, errors.New("CPU sampling unavailable on this OS")
}
