package qemu

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
)

// ringBuffer is a fixed-capacity, line-oriented buffer used to answer Readlog
// requests (the last N lines of guest console output). It is safe for
// concurrent use.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer {
	if max <= 0 {
		max = 150
	}
	return &ringBuffer{max: max}
}

func (r *ringBuffer) push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ringBuffer) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// pushConsole forwards a chunk of output to both the registered log callback and
// the rolling log buffer. Newlines are split so the buffer stays line-oriented.
func (e *Environment) pushConsole(s string) {
	e.logCallbackMx.Lock()
	cb := e.logCallback
	e.logCallbackMx.Unlock()
	if cb != nil {
		cb([]byte(s))
	}
	for _, line := range strings.Split(strings.TrimRight(s, "\r\n"), "\n") {
		e.logBuf.push(strings.TrimRight(line, "\r"))
	}
}

// Attach connects to the guest's serial console (a unix socket bound by libvirt
// when the domain started) and streams its output to the log callback. It also
// starts the resource polling loop. It is guarded so a second concurrent call is
// a no-op rather than racing for ownership of the connection.
func (e *Environment) Attach(ctx context.Context) error {
	// Guard against double-attach (rapid power cycles / reconnects).
	e.consoleMu.Lock()
	if e.consoleAttached {
		e.consoleMu.Unlock()
		return nil
	}
	e.consoleAttached = true
	e.consoleMu.Unlock()

	// Background goroutines run on the per-run context created by Start so they
	// are torn down when the VM goes offline.
	e.mu.RLock()
	pctx := e.procCtx
	e.mu.RUnlock()
	if pctx == nil {
		pctx = context.Background()
	}

	if e.meta.guestOS() != OSLinux {
		// Windows (and other graphical-only) guests have no usable text serial
		// console; their output is via the VNC graphical console instead. We
		// still kick off resource polling so stats are reported.
		go e.pollResources(pctx)
		return nil
	}

	conn, err := dialWithRetry(ctx, e.consoleSocketPath(), 15, 500*time.Millisecond)
	if err != nil {
		e.consoleMu.Lock()
		e.consoleAttached = false
		e.consoleMu.Unlock()
		return errors.Wrap(err, "qemu: failed to attach to guest serial console")
	}

	e.consoleMu.Lock()
	e.console = conn
	e.consoleMu.Unlock()

	// Stream console output until the connection closes.
	go func() {
		defer func() {
			e.consoleMu.Lock()
			if e.console == conn {
				e.console = nil
			}
			e.consoleAttached = false
			e.consoleMu.Unlock()
			_ = conn.Close()
		}()

		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			e.logBuf.push(line)
			e.logCallbackMx.Lock()
			cb := e.logCallback
			e.logCallbackMx.Unlock()
			if cb != nil {
				cb([]byte(line + "\n"))
			}
		}
		if err := scanner.Err(); err != nil {
			e.log().WithField("error", err).Debug("guest console reader stopped")
		}
	}()

	// Resource polling runs on the per-run context.
	go e.pollResources(pctx)

	return nil
}

// IsAttached reports whether a console connection is currently open.
func (e *Environment) IsAttached() bool {
	e.consoleMu.Lock()
	defer e.consoleMu.Unlock()
	return e.console != nil
}

// SendCommand writes a line to the guest serial console. This requires the guest
// to have a getty/login (or application) listening on the serial port.
func (e *Environment) SendCommand(c string) error {
	e.consoleMu.Lock()
	conn := e.console
	e.consoleMu.Unlock()
	if conn == nil {
		return errors.New("qemu: cannot send command, no console connection is attached")
	}
	if _, err := conn.Write([]byte(c + "\n")); err != nil {
		return errors.Wrap(err, "qemu: failed to write to guest console")
	}
	return nil
}

// Readlog returns the last lines lines of buffered console output.
func (e *Environment) Readlog(lines int) ([]string, error) {
	return e.logBuf.tail(lines), nil
}

// dialWithRetry repeatedly attempts to connect to a unix socket, since the
// socket may not exist for a brief moment after the domain is started.
func dialWithRetry(ctx context.Context, path string, attempts int, delay time.Duration) (net.Conn, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(delay)
	}
	return nil, lastErr
}
