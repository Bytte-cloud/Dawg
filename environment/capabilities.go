package environment

// Capabilities describes the optional features a particular environment backend
// supports. Different backends (Docker containers vs. QEMU virtual machines)
// expose very different surfaces — a container has a host-managed filesystem and
// a text stdio console, whereas a virtual machine owns its own disk and may only
// be reachable over a graphical (VNC) console.
//
// Callers (the HTTP router, websocket handlers, etc.) should gate features on
// these capabilities rather than hard-coding assumptions that only hold for the
// Docker backend.
type Capabilities struct {
	// Filesystem indicates that the host can directly read and write the
	// server's files (and therefore that the SFTP server and file manager are
	// usable). This is true for Docker (bind-mounted volume) and false for a
	// running VM whose disk is owned by the guest operating system.
	Filesystem bool

	// TextConsole indicates that the environment exposes a line-oriented
	// stdin/stdout console that can be streamed over the standard console
	// websocket and written to via SendCommand.
	TextConsole bool

	// GraphicalConsole indicates that the environment exposes a graphical
	// framebuffer console (e.g. VNC/SPICE) that must be proxied separately from
	// the text console.
	GraphicalConsole bool

	// LiveResize indicates that resource limits (CPU/memory) can be applied to a
	// running instance without a restart via InSituUpdate.
	LiveResize bool
}

// CapableEnvironment is implemented by environments that can describe their own
// capabilities. Environments that do not implement it are assumed to behave like
// the original Docker backend (see DefaultCapabilities).
type CapableEnvironment interface {
	Capabilities() Capabilities
}

// DefaultCapabilities returns the capability set assumed for environments that do
// not implement CapableEnvironment. These match the behaviour the daemon relied
// on before multiple backends existed (the Docker container backend).
func DefaultCapabilities() Capabilities {
	return Capabilities{
		Filesystem:       true,
		TextConsole:      true,
		GraphicalConsole: false,
		LiveResize:       true,
	}
}

// GetCapabilities returns the capabilities for the provided environment,
// falling back to DefaultCapabilities for backends that do not advertise their
// own. This is the function callers should use.
func GetCapabilities(e ProcessEnvironment) Capabilities {
	if c, ok := e.(CapableEnvironment); ok {
		return c.Capabilities()
	}
	return DefaultCapabilities()
}
