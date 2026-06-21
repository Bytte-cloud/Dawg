package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"emperror.dev/errors"
)

// driver is a thin wrapper around the host `virsh` and `qemu-img` binaries. It
// intentionally shells out rather than linking libvirt via cgo so that Wings can
// continue to be built with CGO_ENABLED=0 and cross-compiled for multiple
// architectures (the official Dockerfile relies on this).
//
// A future hardening step is to replace this with the pure-Go libvirt RPC client
// (github.com/digitalocean/go-libvirt); the surface is deliberately small and
// isolated here to make that swap straightforward.
type driver struct {
	// uri is the libvirt connection URI (e.g. "qemu:///system").
	uri string
}

func newDriver(uri string) *driver {
	if uri == "" {
		uri = "qemu:///system"
	}
	return &driver{uri: uri}
}

// run executes a command, returning trimmed stdout. Any stderr output is folded
// into the returned error to make debugging on the host easier.
func (d *driver) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return strings.TrimSpace(stdout.String()), errors.Wrapf(err, "qemu/driver: command %q failed: %s", name+" "+strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// virsh runs a virsh subcommand against the configured connection URI.
func (d *driver) virsh(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-c", d.uri}, args...)
	return d.run(ctx, "virsh", full...)
}

// ---------------------------------------------------------------------------
// Domain lifecycle
// ---------------------------------------------------------------------------

// DefineFromFile defines (registers) a persistent domain from an XML file.
func (d *driver) DefineFromFile(ctx context.Context, xmlPath string) error {
	_, err := d.virsh(ctx, "define", xmlPath)
	return err
}

// Start boots a defined domain.
func (d *driver) Start(ctx context.Context, name string) error {
	_, err := d.virsh(ctx, "start", name)
	return err
}

// Shutdown requests a graceful ACPI shutdown of the guest. This is best-effort:
// the guest must honour the ACPI power signal for it to take effect.
func (d *driver) Shutdown(ctx context.Context, name string) error {
	_, err := d.virsh(ctx, "shutdown", name)
	return err
}

// Destroy forcibly powers off a running domain (equivalent to pulling the plug).
func (d *driver) Destroy(ctx context.Context, name string) error {
	_, err := d.virsh(ctx, "destroy", name)
	return err
}

// Undefine removes the persistent definition for a domain. The --nvram flag
// ensures any per-VM UEFI variable store is also removed. Storage is managed by
// Wings directly and is therefore intentionally not removed here.
func (d *driver) Undefine(ctx context.Context, name string) error {
	_, err := d.virsh(ctx, "undefine", name, "--nvram")
	return err
}

// State returns the libvirt domain state string, e.g. "running", "shut off",
// "paused", "in shutdown". An empty string is returned if the domain does not
// exist.
func (d *driver) State(ctx context.Context, name string) (string, error) {
	out, err := d.virsh(ctx, "domstate", name)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Exists reports whether a domain with the given name is defined.
func (d *driver) Exists(ctx context.Context, name string) (bool, error) {
	_, err := d.virsh(ctx, "dominfo", name)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DomStats returns the raw `virsh domstats` output for the requested groups.
func (d *driver) DomStats(ctx context.Context, name string) (string, error) {
	return d.virsh(ctx, "domstats", name, "--cpu-total", "--balloon", "--interface", "--block")
}

// GuestInterfaceAddresses returns the raw `virsh domifaddr` output. The source
// is the lease database first, falling back to the guest agent if available.
func (d *driver) GuestInterfaceAddresses(ctx context.Context, name string) (string, error) {
	if out, err := d.virsh(ctx, "domifaddr", name, "--source", "lease"); err == nil && strings.TrimSpace(out) != "" {
		return out, nil
	}
	return d.virsh(ctx, "domifaddr", name, "--source", "agent")
}

// VNCDisplay returns the raw `virsh vncdisplay` output for a domain (e.g. ":0"
// or "127.0.0.1:0"). It is empty when the domain is not running or has no VNC
// graphics device.
func (d *driver) VNCDisplay(ctx context.Context, name string) (string, error) {
	out, err := d.virsh(ctx, "vncdisplay", name)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ---------------------------------------------------------------------------
// Disk image management (qemu-img)
// ---------------------------------------------------------------------------

// CreateOverlay creates a copy-on-write qcow2 overlay backed by base. This is
// how new VMs are provisioned cheaply from a shared template image.
func (d *driver) CreateOverlay(ctx context.Context, base, target string) error {
	_, err := d.run(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, target)
	return err
}

// CreateBlank creates an empty qcow2 disk of the requested size (e.g. "20G").
func (d *driver) CreateBlank(ctx context.Context, target, size string) error {
	_, err := d.run(ctx, "qemu-img", "create", "-f", "qcow2", target, size)
	return err
}

// Resize grows a qcow2 image to the requested size. qemu-img only grows images
// safely; shrinking is rejected by qemu-img and must not be attempted here.
func (d *driver) Resize(ctx context.Context, target, size string) error {
	_, err := d.run(ctx, "qemu-img", "resize", target, size)
	return err
}

// VirtualSize returns the virtual (guest-visible) size of a qcow2 image in
// bytes. Callers use it to avoid attempting a shrink, which qemu-img rejects.
func (d *driver) VirtualSize(ctx context.Context, target string) (int64, error) {
	out, err := d.run(ctx, "qemu-img", "info", "--output=json", target)
	if err != nil {
		return 0, err
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return 0, errors.Wrap(err, "qemu/driver: failed to parse qemu-img info output")
	}
	return info.VirtualSize, nil
}

// isNotFound returns true when an error from virsh indicates the domain does not
// exist, which several callers treat as a non-error condition.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "failed to get domain") ||
		strings.Contains(s, "domain not found") ||
		strings.Contains(s, "no domain with matching name")
}
