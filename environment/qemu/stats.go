package qemu

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/pterodactyl/wings/environment"
)

// pollResources polls libvirt for the VM's resource usage on a fixed interval
// and publishes environment.Stats via the ResourceEvent, matching the Docker
// backend's behaviour. It also detects a guest that powers itself off (the VM
// equivalent of a process exiting) and transitions the environment offline so
// state tracking and crash detection behave. It returns when the context is
// cancelled or the VM goes offline.
func (e *Environment) pollResources(ctx context.Context) {
	if e.State() == environment.ProcessOfflineState {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var (
		lastCPU  uint64
		lastTime time.Time
		haveLast bool
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e.State() == environment.ProcessOfflineState {
				return
			}

			// Detect a guest that shut itself down (or crashed). When the domain
			// is no longer running but we still think it is, transition offline
			// so listeners + crash detection react.
			state, sErr := e.driver.State(ctx, e.domainName())
			if sErr == nil && state != "running" {
				if cur := e.State(); cur == environment.ProcessRunningState || cur == environment.ProcessStartingState {
					e.SetState(environment.ProcessStoppingState)
					e.SetState(environment.ProcessOfflineState)
				}
				return
			}

			out, err := e.driver.DomStats(ctx, e.domainName())
			if err != nil {
				continue
			}
			m := parseDomStats(out)

			now := time.Now()
			st := environment.Stats{
				MemoryLimit: uint64(memoryMiB(e.Config().Limits())) * 1024 * 1024,
			}

			// Memory: prefer the resident set of the qemu process (actual host
			// memory), falling back to the ballooned guest memory. Both are KiB.
			if rss, ok := m["balloon.rss"]; ok {
				st.Memory = rss * 1024
			} else if cur, ok := m["balloon.current"]; ok {
				st.Memory = cur * 1024
			}

			// CPU: cpu.time is cumulative nanoseconds across all vCPUs. Convert
			// the delta over the elapsed interval into a percentage where 100 ==
			// one fully-utilised core (so a busy 2-vCPU VM reports ~200), which
			// matches the Docker backend's CpuAbsolute semantics.
			if cpu, ok := m["cpu.time"]; ok {
				if haveLast {
					elapsed := now.Sub(lastTime).Nanoseconds()
					if elapsed > 0 && cpu >= lastCPU {
						st.CpuAbsolute = (float64(cpu-lastCPU) / float64(elapsed)) * 100
					}
				}
				lastCPU = cpu
				lastTime = now
				haveLast = true
			}

			// Network: sum rx/tx across all interfaces.
			ifCount := m["net.count"]
			for i := uint64(0); i < ifCount; i++ {
				p := "net." + strconv.FormatUint(i, 10)
				st.Network.RxBytes += m[p+".rx.bytes"]
				st.Network.TxBytes += m[p+".tx.bytes"]
			}

			if up, err := e.Uptime(ctx); err == nil {
				st.Uptime = up
			}

			e.Events().Publish(environment.ResourceEvent, st)
		}
	}
}

// parseDomStats parses the key=value output of `virsh domstats` into a map. Only
// unsigned-integer values are retained (every field we consume is numeric).
func parseDomStats(out string) map[string]uint64 {
	m := make(map[string]uint64)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if n, err := strconv.ParseUint(val, 10, 64); err == nil {
			m[key] = n
		}
	}
	return m
}
