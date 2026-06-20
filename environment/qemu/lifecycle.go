package qemu

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
)

// enabled returns an error if the QEMU backend is not enabled on this node.
func (e *Environment) enabled() error {
	if !config.Get().Qemu.Enabled {
		return errors.New("qemu: virtualization backend is not enabled on this node (set qemu.enabled: true in the Wings config)")
	}
	return nil
}

// macAddress derives a stable, locally-administered MAC address from the server
// UUID so the guest keeps the same address across reboots (which keeps DHCP
// leases and port-forwards stable). Uses the QEMU OUI prefix 52:54:00.
func (e *Environment) macAddress() string {
	sum := sha256.Sum256([]byte(e.Id))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// buildSpec assembles a DomainSpec from the daemon config, the server limits and
// the VM metadata.
func (e *Environment) buildSpec() DomainSpec {
	qc := config.Get().Qemu
	l := e.Config().Limits()

	spec := DomainSpec{
		Name:            e.domainName(),
		VCPUs:           vcpus(l),
		MemoryMiB:       memoryMiB(l),
		CPUQuotaPercent: cpuQuotaPercent(l),
		UseKVM:          qc.EnableKVM,
		Windows:         e.meta.guestOS() == OSWindows,
		DiskPath:        e.diskPath(),
		Bridge:          qc.Bridge,
		MAC:             e.macAddress(),
		ConsoleSocket:   e.consoleSocketPath(),
		VNCListen:       qc.VNCBindAddress,
	}

	if e.meta.useUEFI() {
		spec.UEFI = true
		spec.OVMFCode = qc.OVMFCodePath
		spec.NVRAMPath = e.nvramPath()
	}

	if e.meta.guestOS() == OSWindows {
		// Driver-free defaults so a stock Windows template boots without virtio
		// drivers pre-installed. Operators can switch to virtio for performance
		// once drivers are present.
		spec.DiskBus = "sata"
		spec.NetModel = "e1000e"
		spec.EnableTPM = true
	} else {
		spec.DiskBus = "virtio"
		spec.NetModel = "virtio"
		// The cloud-init seed is generated during Create for Linux guests.
		if _, err := os.Stat(e.seedISOPath()); err == nil {
			spec.SeedISOPath = e.seedISOPath()
		}
	}

	return spec
}

// Create provisions the virtual machine: it builds the disk overlay, generates
// any firmware/provisioning media, renders the domain XML and defines the domain
// with libvirt. This doubles as the "installation" step for a VM server. It is
// idempotent — if the domain is already defined it returns immediately.
func (e *Environment) Create() error {
	if err := e.enabled(); err != nil {
		return err
	}

	ctx := context.Background()

	if err := os.MkdirAll(e.dataDir(), 0o700); err != nil {
		return errors.Wrap(err, "qemu: failed to create VM data directory")
	}

	// If the domain is already defined there is nothing to do.
	if exists, err := e.driver.Exists(ctx, e.domainName()); err != nil {
		return err
	} else if exists {
		return nil
	}

	// 1. Disk overlay from the template image.
	if err := e.provisionDisk(ctx); err != nil {
		return err
	}

	// 2. Per-VM UEFI NVRAM store (copied from the OVMF vars template).
	if e.meta.useUEFI() {
		if err := copyFile(config.Get().Qemu.OVMFVarsPath, e.nvramPath()); err != nil {
			return errors.Wrap(err, "qemu: failed to create per-VM NVRAM store")
		}
	}

	// 3. Provisioning media (cloud-init for Linux).
	if _, err := e.buildSeedISO(ctx); err != nil {
		return err
	}

	// 4. Render + write the domain definition.
	spec := e.buildSpec()
	xml, err := spec.XML()
	if err != nil {
		return errors.Wrap(err, "qemu: failed to render domain XML")
	}
	xmlPath := filepath.Join(e.dataDir(), "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(xml), 0o600); err != nil {
		return errors.Wrap(err, "qemu: failed to write domain XML")
	}

	// 5. Define the domain with libvirt.
	if err := e.driver.DefineFromFile(ctx, xmlPath); err != nil {
		return err
	}

	return nil
}

// Destroy powers off (if necessary) and removes the VM along with its storage.
func (e *Environment) Destroy() error {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	ctx := context.Background()
	e.SetState(environment.ProcessStoppingState)

	if running, _ := e.IsRunning(ctx); running {
		_ = e.driver.Destroy(ctx, e.domainName())
	}
	_ = e.removePortForwards(ctx)

	if exists, _ := e.driver.Exists(ctx, e.domainName()); exists {
		if err := e.driver.Undefine(ctx, e.domainName()); err != nil {
			return err
		}
	}

	if err := e.removeStorage(); err != nil {
		e.log().WithField("error", err).Warn("failed to remove VM storage during destroy")
	}

	e.SetState(environment.ProcessOfflineState)
	return nil
}

// Exists reports whether the libvirt domain is defined.
func (e *Environment) Exists() (bool, error) {
	return e.driver.Exists(context.Background(), e.domainName())
}

// IsRunning reports whether the domain is currently running.
func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	state, err := e.driver.State(ctx, e.domainName())
	if err != nil {
		return false, err
	}
	return state == "running", nil
}

// InSituUpdate is a no-op for QEMU. Changing CPU/memory limits requires a
// reboot (the daemon does not currently hotplug resources), so the new limits
// take effect on the next start.
func (e *Environment) InSituUpdate() error {
	return nil
}

// Uptime returns the milliseconds elapsed since the VM was last started, or 0
// when it is not running.
func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	if running, err := e.IsRunning(ctx); err != nil || !running {
		return 0, err
	}
	e.mu.RLock()
	started := e.startedAt
	e.mu.RUnlock()
	if started.IsZero() {
		return 0, nil
	}
	return time.Since(started).Milliseconds(), nil
}

// ExitState returns the exit code of the last run and whether it was OOM-killed.
// QEMU does not surface a guest exit code the way a container does, so this
// reports the last tracked values (0 / false by default).
func (e *Environment) ExitState() (uint32, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.exitCode, e.oomKilled, nil
}

// copyFile copies src to dst, creating parent directories as needed. It is a
// no-op if dst already exists.
func copyFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
