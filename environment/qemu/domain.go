package qemu

import (
	"strconv"

	"github.com/beevik/etree"
)

// DomainSpec is the fully-resolved description of a virtual machine from which a
// libvirt domain XML document is generated. All host-specific paths and derived
// resource values are computed by the caller; this type is purely declarative.
type DomainSpec struct {
	Name      string
	VCPUs     int
	MemoryMiB int64

	// CPUQuotaPercent caps host CPU usage (100 == one full core). Zero is
	// uncapped.
	CPUQuotaPercent int64

	// UseKVM enables hardware acceleration. When false the (much slower) TCG
	// software emulator is used.
	UseKVM bool

	// Windows toggles guest-OS specific tweaks (RTC localtime, Hyper-V
	// enlightenments, a vTPM when EnableTPM is set).
	Windows bool

	// UEFI selects OVMF firmware. OVMFCode/NVRAMPath must be set when true.
	UEFI      bool
	OVMFCode  string
	NVRAMPath string
	EnableTPM bool

	// Disk.
	DiskPath string
	DiskBus  string // "virtio" | "sata"

	// SeedISOPath, when set, is attached as a read-only cloud-init datasource
	// (NoCloud) CD-ROM.
	SeedISOPath string

	// Networking.
	Bridge   string
	MAC      string
	NetModel string // "virtio" | "e1000e"

	// ConsoleSocket is the host path of the unix socket bound to the guest's
	// first serial port.
	ConsoleSocket string

	// VNCListen is the address the graphical console binds to on the host.
	VNCListen string
}

// XML renders the libvirt domain definition for this spec. etree handles all
// attribute/text escaping, so untrusted values (names, paths) cannot break the
// document structure.
func (s DomainSpec) XML() (string, error) {
	doc := etree.NewDocument()
	doc.Indent(2)

	domainType := "kvm"
	if !s.UseKVM {
		domainType = "qemu"
	}

	domain := doc.CreateElement("domain")
	domain.CreateAttr("type", domainType)

	domain.CreateElement("name").SetText(s.Name)

	mem := domain.CreateElement("memory")
	mem.CreateAttr("unit", "MiB")
	mem.SetText(strconv.FormatInt(s.MemoryMiB, 10))
	cur := domain.CreateElement("currentMemory")
	cur.CreateAttr("unit", "MiB")
	cur.SetText(strconv.FormatInt(s.MemoryMiB, 10))

	domain.CreateElement("vcpu").SetText(strconv.Itoa(s.VCPUs))

	// Apply a CPU cap via cputune when the panel set a limit. period is fixed at
	// the conventional 100ms; quota = period * (percent/100).
	if s.CPUQuotaPercent > 0 {
		const period = 100000
		quota := (int64(period) * s.CPUQuotaPercent) / 100
		ct := domain.CreateElement("cputune")
		ct.CreateElement("period").SetText(strconv.Itoa(period))
		ct.CreateElement("quota").SetText(strconv.FormatInt(quota, 10))
	}

	// OS / firmware.
	osEl := domain.CreateElement("os")
	osType := osEl.CreateElement("type")
	osType.CreateAttr("arch", "x86_64")
	osType.CreateAttr("machine", "q35")
	osType.SetText("hvm")
	if s.UEFI {
		loader := osEl.CreateElement("loader")
		loader.CreateAttr("readonly", "yes")
		loader.CreateAttr("type", "pflash")
		loader.SetText(s.OVMFCode)
		if s.NVRAMPath != "" {
			osEl.CreateElement("nvram").SetText(s.NVRAMPath)
		}
	}
	boot := osEl.CreateElement("boot")
	boot.CreateAttr("dev", "hd")
	if s.SeedISOPath != "" || s.Windows {
		// Allow falling back to CD-ROM boot for installer/seed media.
		b2 := osEl.CreateElement("boot")
		b2.CreateAttr("dev", "cdrom")
	}

	// Features.
	features := domain.CreateElement("features")
	features.CreateElement("acpi")
	features.CreateElement("apic")
	if s.Windows {
		hv := features.CreateElement("hyperv")
		for _, f := range []string{"relaxed", "vapic", "spinlocks"} {
			el := hv.CreateElement(f)
			el.CreateAttr("state", "on")
			if f == "spinlocks" {
				el.CreateAttr("retries", "8191")
			}
		}
	}

	// Use the host CPU model for best performance under KVM. host-passthrough
	// requires KVM; under TCG (software emulation, no /dev/kvm) it cannot start,
	// so fall back to a generic model qemu can emulate.
	cpu := domain.CreateElement("cpu")
	if s.UseKVM {
		cpu.CreateAttr("mode", "host-passthrough")
		cpu.CreateAttr("check", "none")
	} else {
		cpu.CreateAttr("mode", "custom")
		cpu.CreateAttr("match", "exact")
		cpu.CreateElement("model").SetText("qemu64")
	}

	// Clock: Windows expects localtime, everything else UTC.
	clock := domain.CreateElement("clock")
	if s.Windows {
		clock.CreateAttr("offset", "localtime")
	} else {
		clock.CreateAttr("offset", "utc")
	}

	domain.CreateElement("on_poweroff").SetText("destroy")
	domain.CreateElement("on_reboot").SetText("restart")
	domain.CreateElement("on_crash").SetText("destroy")

	devices := domain.CreateElement("devices")

	// Primary disk.
	disk := devices.CreateElement("disk")
	disk.CreateAttr("type", "file")
	disk.CreateAttr("device", "disk")
	dDriver := disk.CreateElement("driver")
	dDriver.CreateAttr("name", "qemu")
	dDriver.CreateAttr("type", "qcow2")
	dDriver.CreateAttr("discard", "unmap")
	disk.CreateElement("source").CreateAttr("file", s.DiskPath)
	dTarget := disk.CreateElement("target")
	if s.DiskBus == "virtio" {
		dTarget.CreateAttr("dev", "vda")
		dTarget.CreateAttr("bus", "virtio")
	} else {
		dTarget.CreateAttr("dev", "sda")
		dTarget.CreateAttr("bus", "sata")
	}

	// Cloud-init seed CD-ROM (Linux provisioning).
	if s.SeedISOPath != "" {
		cd := devices.CreateElement("disk")
		cd.CreateAttr("type", "file")
		cd.CreateAttr("device", "cdrom")
		cdDriver := cd.CreateElement("driver")
		cdDriver.CreateAttr("name", "qemu")
		cdDriver.CreateAttr("type", "raw")
		cd.CreateElement("source").CreateAttr("file", s.SeedISOPath)
		cdTarget := cd.CreateElement("target")
		cdTarget.CreateAttr("dev", "sdc")
		cdTarget.CreateAttr("bus", "sata")
		cd.CreateElement("readonly")
	}

	// Network interface attached to the host bridge. libvirt creates and manages
	// the tap device for us.
	iface := devices.CreateElement("interface")
	iface.CreateAttr("type", "bridge")
	iface.CreateElement("source").CreateAttr("bridge", s.Bridge)
	if s.MAC != "" {
		iface.CreateElement("mac").CreateAttr("address", s.MAC)
	}
	model := iface.CreateElement("model")
	if s.NetModel == "" {
		model.CreateAttr("type", "virtio")
	} else {
		model.CreateAttr("type", s.NetModel)
	}

	// Serial console bound to a host unix socket so Wings can attach to it.
	serial := devices.CreateElement("serial")
	serial.CreateAttr("type", "unix")
	sSource := serial.CreateElement("source")
	sSource.CreateAttr("mode", "bind")
	sSource.CreateAttr("path", s.ConsoleSocket)
	serial.CreateElement("target").CreateAttr("port", "0")

	// QEMU guest agent channel (used for graceful operations + IP discovery).
	channel := devices.CreateElement("channel")
	channel.CreateAttr("type", "unix")
	channel.CreateElement("source").CreateAttr("mode", "bind")
	cTarget := channel.CreateElement("target")
	cTarget.CreateAttr("type", "virtio")
	cTarget.CreateAttr("name", "org.qemu.guest_agent.0")

	// Graphical console (VNC) — required for Windows, useful for Linux installs.
	graphics := devices.CreateElement("graphics")
	graphics.CreateAttr("type", "vnc")
	graphics.CreateAttr("port", "-1")
	graphics.CreateAttr("autoport", "yes")
	if s.VNCListen != "" {
		// libvirt requires the <listen> element to carry an explicit type;
		// without type="address" it rejects the domain at define time.
		listen := graphics.CreateElement("listen")
		listen.CreateAttr("type", "address")
		listen.CreateAttr("address", s.VNCListen)
	}
	video := devices.CreateElement("video")
	video.CreateElement("model").CreateAttr("type", "qxl")

	// USB tablet gives a usable pointer in the graphical console.
	tablet := devices.CreateElement("input")
	tablet.CreateAttr("type", "tablet")
	tablet.CreateAttr("bus", "usb")

	devices.CreateElement("memballoon").CreateAttr("model", "virtio")

	// Virtual TPM for guests that require it (e.g. Windows 11).
	if s.EnableTPM {
		tpm := devices.CreateElement("tpm")
		tpm.CreateAttr("model", "tpm-crb")
		backend := tpm.CreateElement("backend")
		backend.CreateAttr("type", "emulator")
		backend.CreateAttr("version", "2.0")
	}

	return doc.WriteToString()
}
