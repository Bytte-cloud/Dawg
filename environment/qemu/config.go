package qemu

import (
	"fmt"
	"math"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
)

// Guest operating-system families. These influence firmware selection (UEFI vs
// BIOS), the console model (serial text vs VNC graphical), and how the guest is
// provisioned (cloud-init vs cloudbase-init/unattend).
const (
	OSLinux   = "linux"
	OSWindows = "windows"
)

// Metadata carries the VM-specific configuration that is not already expressed by
// the shared environment.Configuration (which only covers limits, allocations,
// mounts, labels and env vars). It is the QEMU analogue of docker.Metadata and
// is supplied by the server manager when constructing the environment.
type Metadata struct {
	// Image is a reference to the base/template disk image the VM is cloned
	// from. This may be an absolute path to a qcow2 file already present in the
	// template directory, the bare filename of such a template, or an
	// http(s):// URL that will be downloaded into the template directory on
	// first provision.
	Image string

	// OS is the guest operating-system family (OSLinux or OSWindows). It
	// defaults to Linux when empty.
	OS string

	// Firmware selects "uefi" or "bios". When empty it is inferred from the OS
	// (Windows defaults to UEFI, Linux to BIOS).
	Firmware string

	// Stop is retained for interface parity with the Docker backend. Virtual
	// machines are always stopped via an ACPI shutdown so the value is unused,
	// but keeping it avoids surprising callers that sync stop configuration.
	Stop remote.ProcessStopConfiguration
}

// guestOS returns the normalised OS family, defaulting to Linux.
func (m *Metadata) guestOS() string {
	if m.OS == OSWindows {
		return OSWindows
	}
	return OSLinux
}

// useUEFI reports whether the VM should boot with UEFI firmware (OVMF).
func (m *Metadata) useUEFI() bool {
	switch m.Firmware {
	case "uefi":
		return true
	case "bios":
		return false
	default:
		// Windows guests effectively require UEFI for modern releases.
		return m.guestOS() == OSWindows
	}
}

// vcpus derives a vCPU count from the panel CPU limit, which is expressed as a
// percentage where 100 == one core. A VM must have at least one vCPU; an
// unlimited (0) panel value maps to a single vCPU.
func vcpus(l environment.Limits) int {
	if l.CpuLimit <= 0 {
		return 1
	}
	n := int(math.Ceil(float64(l.CpuLimit) / 100.0))
	if n < 1 {
		n = 1
	}
	return n
}

// memoryMiB returns the guest memory allocation in MiB, with a sane floor.
func memoryMiB(l environment.Limits) int64 {
	if l.MemoryLimit < 256 {
		return 512
	}
	return l.MemoryLimit
}

// diskSizeString returns a qemu-img compatible size string derived from the
// panel disk limit (which is in MB). A floor of 1 GiB is enforced.
func diskSizeString(l environment.Limits) string {
	mb := l.DiskSpace
	if mb < 1024 {
		mb = 1024
	}
	return fmt.Sprintf("%dM", mb)
}

// cpuQuotaPercent returns the host CPU cap as a percentage for the cgroup that
// libvirt applies to the VM (mirrors the panel's CpuLimit). Zero means uncapped.
func cpuQuotaPercent(l environment.Limits) int64 {
	if l.CpuLimit <= 0 {
		return 0
	}
	return l.CpuLimit
}
