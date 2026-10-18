package system

import (
	"math"
	"runtime"
	"sync"
	"time"
)

type CPU struct {
	mu                      sync.Mutex
	last                    time.Time
	process, total, idle    float64
	systemLoad, processLoad float64
	available               bool
}

func (c *CPU) Sample() {
	p, t, i, e := cpuTimes()
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e != nil {
		c.available = false
		return
	}
	if !c.last.IsZero() {
		elapsed := now.Sub(c.last).Seconds()
		if elapsed > 0 {
			c.processLoad = clamp((p - c.process) / elapsed / float64(runtime.NumCPU()))
		}
		if t > c.total {
			c.systemLoad = clamp(1 - (i-c.idle)/(t-c.total))
		}
		c.available = true
	}
	c.last = now
	c.process = p
	c.total = t
	c.idle = i
}
func (c *CPU) Values() (float64, float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.systemLoad, c.processLoad, c.available
}
func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }
