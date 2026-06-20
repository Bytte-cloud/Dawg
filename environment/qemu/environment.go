package qemu

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/system"
)

// Ensure that the QEMU environment always implements the full environment
// interface (and advertises its capabilities).
var (
	_ environment.ProcessEnvironment = (*Environment)(nil)
	_ environment.CapableEnvironment = (*Environment)(nil)
)

// Environment implements environment.ProcessEnvironment for QEMU/KVM virtual
// machines. It mirrors the structure of environment/docker as closely as
// possible so that the rest of Wings can treat a VM like any other server.
type Environment struct {
	mu sync.RWMutex

	// opMu serializes power/lifecycle operations (Start/Stop/WaitForStop/
	// Terminate/Destroy) so they cannot interleave and corrupt the state
	// machine. It is separate from mu, which only guards field access.
	opMu sync.Mutex

	// Id is the public identifier for this environment — the server UUID. It is
	// used to derive the libvirt domain name and on-disk paths.
	Id string

	// Configuration is the shared environment configuration (limits,
	// allocations, mounts, labels, env vars) supplied by the server.
	Configuration *environment.Configuration

	meta   *Metadata
	driver *driver

	emitter *events.Bus

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	// console holds the connection to the guest serial console (a unix socket
	// bound by libvirt) and a rolling buffer of recent output for Readlog.
	// consoleAttached guards against two concurrent Attach calls racing to own
	// the connection.
	consoleMu       sync.Mutex
	console         net.Conn
	consoleAttached bool
	logBuf          *ringBuffer

	// procCtx/procCancel scope the per-run background goroutines (resource
	// poller, console reader, port-forward waiter). They are recreated on each
	// Start and cancelled when the VM transitions offline so nothing leaks
	// across power cycles.
	procCtx    context.Context
	procCancel context.CancelFunc

	// st tracks the environment state (offline/starting/running/stopping).
	st *system.AtomicString

	// startedAt records the wall-clock time the VM was last started, used to
	// derive uptime (libvirt does not expose a simple uptime value).
	startedAt time.Time

	// lastGuestIP records the most recent DHCP address the guest held, so that
	// port-forward rules for a prior address can be torn down even after the
	// guest's lease changes.
	lastGuestIP string

	// exitCode / oomKilled record the result of the last run for ExitState.
	exitCode  uint32
	oomKilled bool
}

// New creates a new QEMU environment. The signature intentionally matches
// docker.New so the two backends are interchangeable from the manager's point
// of view.
func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	if m == nil {
		m = &Metadata{}
	}
	e := &Environment{
		Id:            id,
		Configuration: c,
		meta:          m,
		driver:        newDriver(config.Get().Qemu.URI),
		emitter:       events.NewBus(),
		logBuf:        newRingBuffer(config.Get().System.WebsocketLogCount),
		st:            system.NewAtomicString(environment.ProcessOfflineState),
	}
	return e, nil
}

func (e *Environment) log() *log.Entry {
	return log.WithField("environment", e.Type()).WithField("vm", e.Id)
}

// Type returns the environment type identifier.
func (e *Environment) Type() string {
	return "qemu"
}

// Capabilities advertises what this backend supports so the router/websocket
// layers can gate features (the file manager and SFTP do not apply to a running
// VM, and Windows guests are graphical-console only).
func (e *Environment) Capabilities() environment.Capabilities {
	caps := environment.Capabilities{
		Filesystem:       false,
		TextConsole:      true,
		GraphicalConsole: true,
		LiveResize:       false,
	}
	if e.meta.guestOS() == OSWindows {
		// Windows guests do not expose a usable text serial console out of the
		// box; their console is graphical (VNC).
		caps.TextConsole = false
	}
	return caps
}

// Config returns the environment configuration.
func (e *Environment) Config() *environment.Configuration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Configuration
}

// Events returns the event bus for this environment.
func (e *Environment) Events() *events.Bus {
	return e.emitter
}

// State returns the current environment state.
func (e *Environment) State() string {
	return e.st.Load()
}

// SetState updates the environment state, emitting a StateChangeEvent when the
// value actually changes. It panics on an invalid state, matching the Docker
// backend's strictness.
func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(errors.New(fmt.Sprintf("qemu: invalid server state received: %s", state)))
	}

	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}

	// Whenever the VM becomes offline, tear down the per-run background
	// goroutines so they do not leak or keep polling a dead domain.
	if state == environment.ProcessOfflineState {
		e.stopBackground()
	}
}

// SetLogCallback registers the callback that receives raw console output.
func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	defer e.logCallbackMx.Unlock()
	e.logCallback = f
}

// ---------------------------------------------------------------------------
// Background goroutine lifecycle
// ---------------------------------------------------------------------------

// newProcContext cancels any previous per-run context and returns a fresh one
// that scopes the goroutines for this run.
func (e *Environment) newProcContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.procCancel != nil {
		e.procCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.procCtx = ctx
	e.procCancel = cancel
	return ctx
}

// stopBackground cancels the per-run goroutines and closes the console
// connection (which unblocks the console reader).
func (e *Environment) stopBackground() {
	e.mu.Lock()
	cancel := e.procCancel
	e.procCancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	e.consoleMu.Lock()
	if e.console != nil {
		_ = e.console.Close()
		e.console = nil
	}
	e.consoleAttached = false
	e.consoleMu.Unlock()
}

// ---------------------------------------------------------------------------
// Filesystem layout helpers
// ---------------------------------------------------------------------------

// domainName is the libvirt domain name for this VM. It is prefixed so the name
// always begins with a letter (libvirt is strict about this) and so VM domains
// are easy to distinguish from anything else on the host.
func (e *Environment) domainName() string {
	return "wings-" + e.Id
}

// dataDir is the per-VM directory holding the disk, NVRAM, seed ISO and console
// socket.
func (e *Environment) dataDir() string {
	return filepath.Join(config.Get().Qemu.DataDirectory, e.Id)
}

func (e *Environment) diskPath() string          { return filepath.Join(e.dataDir(), "disk.qcow2") }
func (e *Environment) nvramPath() string         { return filepath.Join(e.dataDir(), "nvram.fd") }
func (e *Environment) seedISOPath() string       { return filepath.Join(e.dataDir(), "seed.iso") }
func (e *Environment) consoleSocketPath() string { return filepath.Join(e.dataDir(), "console.sock") }

// VNCAddress returns the host "host:port" of the running VM's VNC (RFB) server,
// e.g. "127.0.0.1:5900", for the graphical-console proxy. It returns an empty
// string (no error) when the VM is not running or exposes no VNC display.
//
// `virsh vncdisplay` returns either ":N" (host implied) or "host:N", where the
// TCP port is 5900 + N.
func (e *Environment) VNCAddress(ctx context.Context) (string, error) {
	out, err := e.driver.VNCDisplay(ctx, e.domainName())
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}

	// The host is always taken from our own configuration (a loopback address by
	// default), never from the virsh output — we only trust it for the display
	// number. This prevents the proxy from ever being pointed at an arbitrary
	// host (SSRF). The VM's VNC is bound to this same address by the domain XML.
	host := config.Get().Qemu.VNCBindAddress
	if host == "" {
		host = "127.0.0.1"
	}

	display := out
	if i := strings.LastIndex(out, ":"); i >= 0 {
		display = out[i+1:]
	}

	// The display segment may carry trailing data; keep only the leading digits.
	end := 0
	for end < len(display) && display[end] >= '0' && display[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(display[:end])
	if err != nil {
		return "", errors.Wrapf(err, "qemu: could not parse VNC display %q", out)
	}

	return host + ":" + strconv.Itoa(5900+n), nil
}
