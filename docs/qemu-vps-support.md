# QEMU/KVM VPS support (VM backend)

Wings can run a server as a **QEMU/KVM virtual machine** instead of a Docker
container. This is implemented as a second `environment.ProcessEnvironment`
backend (`environment/qemu`) selected per-server, so the rest of the daemon
(power actions, websocket console, stats, crash detection, the panel protocol)
is unchanged.

> Status: **daemon-side core**. Linux VM lifecycle, provisioning (cloud-init),
> networking (NAT port-forwards), serial console and stats are implemented and
> compile for `linux/amd64` (`CGO_ENABLED=0`). It has **not** been runtime-tested
> on a KVM host yet — treat the first boot as a bring-up exercise. See
> [Not yet done](#not-yet-done) for the remaining surface (Windows automation,
> file access, panel UI).

## How a server becomes a VM

The backend is chosen from the server configuration the panel sends:

| Field | Meaning |
|---|---|
| `environment_type: "qemu"` | run as a VM (anything else / omitted → Docker container, unchanged) |
| `container.image` | the **base/template** qcow2 image: an absolute path, a filename under the template dir, or an `http(s)://` URL (downloaded + cached on first provision) |
| label `vm.os` | `linux` (default) or `windows` |
| label `vm.firmware` | `bios` or `uefi` (default: inferred — Windows→UEFI, Linux→BIOS) |
| `build` limits | `cpu_limit` (100 = 1 vCPU), `memory_limit` (MiB), `disk_space` (MB) map to vCPUs/RAM/disk |
| `allocations` | host ports DNAT-forwarded into the guest |
| env `VM_USER` / `VM_PASSWORD` / `VM_SSH_KEYS` / `VM_HOSTNAME` | Linux cloud-init provisioning (a random password is generated + printed to the console if unset) |

No panel changes are required for the daemon to *accept* these — older panels
simply omit `environment_type` and keep getting containers. Wiring the panel UI
to *send* them is a follow-up (see below).

## Daemon configuration (`config.yml`)

```yaml
qemu:
  enabled: true                 # must be true to provision/boot VMs
  uri: "qemu:///system"
  bridge: "virbr0"              # host bridge the VM NIC attaches to
  nameserver: "1.1.1.1"
  enable_kvm: true              # false = slow software emulation (TCG)
  vnc_bind_address: "127.0.0.1"
  data_directory: "/var/lib/pterodactyl/vms"
  template_directory: "/var/lib/pterodactyl/vm-templates"
  ovmf_code_path: "/usr/share/OVMF/OVMF_CODE.fd"
  ovmf_vars_path: "/usr/share/OVMF/OVMF_VARS.fd"
```

## Host requirements

The Wings host (the node running VMs) needs:

- **KVM**: `/dev/kvm` present (hardware virtualization; nested virt if Wings is
  itself in a VM). Without it, set `enable_kvm: false` (much slower).
- **libvirt + tools on `PATH`**: `virsh`, `qemu-img`, `qemu-system-x86_64`,
  `iptables`, and one of `genisoimage` / `mkisofs` / `xorriso` (cloud-init ISO).
  For UEFI/Windows: `OVMF` firmware and `swtpm` (vTPM). For guest IP discovery:
  `qemu-guest-agent` in the guest, or rely on the libvirt DHCP lease database.
- A **network bridge** (`bridge:` above) the VMs attach to. libvirt's default
  `virbr0` (NAT) works out of the box; a routed bridge is needed for public IPs.
- IP forwarding enabled (`net.ipv4.ip_forward=1`) for the NAT port-forwards.

> ⚠️ **LXC/Docker caveat.** Running this backend inside an LXC container (or the
> Wings Docker container) is not recommended — KVM passthrough, libvirtd and
> bridge management inside LXC/Docker are fragile. Run a VM-capable node on bare
> metal or a nested-virt-enabled VM, with Wings on the host (not containerized)
> so it can reach `/dev/kvm` and the libvirt socket.

## How it works

- **Provision (`Create`, also the "install" step)** — resolve/download the
  template, create a copy-on-write qcow2 overlay (`disk.qcow2`) sized to the
  panel disk limit, copy a per-VM UEFI NVRAM store when needed, build the
  cloud-init NoCloud seed ISO (Linux), render the libvirt domain XML and
  `virsh define` it. Idempotent.
- **Power** — `virsh start` / graceful `virsh shutdown` (ACPI) / forced
  `virsh destroy`; state machine matches the Docker backend so crash detection
  behaves.
- **Console** — the guest's first serial port is bound to a host unix socket
  (`console.sock`); Wings dials it and streams to the standard console
  websocket. Linux guests need `console=ttyS0` (cloud-init images have it).
- **Networking** — the NIC attaches to the host bridge (libvirt manages the
  tap). Allocations become `iptables` DNAT + FORWARD rules to the guest's DHCP
  address (discovered via `virsh domifaddr`).
- **Stats** — `virsh domstats` polled every 2s → `environment.Stats`
  (CPU%, memory, network), published on the same `ResourceEvent`.

Per-VM files live under `data_directory/<server-uuid>/`:
`disk.qcow2`, `nvram.fd`, `seed.iso`, `console.sock`, `domain.xml`.

## Capabilities

A new capability model (`environment/capabilities.go`,
`environment.GetCapabilities`) lets callers gate features per backend:

| | Filesystem | TextConsole | GraphicalConsole | LiveResize |
|---|---|---|---|---|
| Docker (default) | ✅ | ✅ | ❌ | ✅ |
| QEMU Linux | ❌ | ✅ (serial) | ✅ (VNC) | ❌ |
| QEMU Windows | ❌ | ❌ | ✅ (VNC) | ❌ |

## Not yet done (follow-ups)

1. **Router/SFTP gating** — the file-manager, SFTP and (for Windows) command
   routes still assume a host-managed filesystem/text console. They should call
   `environment.GetCapabilities(s.Environment)` and return `409` when the
   capability is absent. The capability helper exists; the route wiring does not.
2. **Graphical (VNC) console proxy** — Windows (and Linux installs) need a
   websocket→VNC proxy in Wings and a noVNC viewer in the panel client.
3. **Windows provisioning** — cloud-init is Linux-only; Windows needs
   cloudbase-init or an `unattend.xml`, plus virtio-win for performant disk/net.
   Current Windows defaults boot driver-free (SATA + e1000e).
4. **File access for VMs** — offline browse/edit by mounting the qcow2 via
   `libguestfs`/`qemu-nbd` when stopped; VM backups via qcow2 snapshot/export.
5. **Advanced networking** — routed public-IP-per-VM, bandwidth limits, and
   robust port-forward reconciliation (current cleanup keys off the live guest
   IP, which can change across DHCP leases).
6. **Live resize** — CPU/RAM hotplug via QMP (`InSituUpdate` is currently a
   no-op; new limits apply on next boot).
7. **libvirt hardening** — the backend shells out to `virsh`; swapping to the
   pure-Go `digitalocean/go-libvirt` RPC client would remove the CLI dependency
   and improve error handling. The driver layer (`environment/qemu/driver.go`)
   is isolated to make this localized.
8. **Panel side (Ruff)** — a VM server type, the VM config form, the noVNC
   console, and capability-aware UI. Intentionally **not** touched in this PR.

## Hardening applied & known limitations

This backend went through an adversarial code review. Fixed in this PR:

- **Goroutine lifecycle** — the resource poller, console reader and port-forward
  waiter are scoped to a per-run context that is cancelled when the VM goes
  offline; `Attach` is guarded against double-attach.
- **Power-operation serialization** — `Start/Stop/WaitForStop/Terminate/Destroy`
  hold a single operation mutex so they cannot interleave and corrupt the state
  machine.
- **Guest-shutdown detection** — the poller transitions the environment offline
  when the guest powers itself off (so state tracking / listeners react).
- **Security** — template path-traversal is blocked; image downloads are size-
  and timeout-bounded; cloud-init is generated via YAML marshalling (no
  injection via hostname/nameserver); provisioning **requires** `VM_PASSWORD`
  and/or `VM_SSH_KEYS` rather than generating and logging a secret; the libvirt
  domain name is prefixed so it is always valid.
- **Router gating** — file-manager, backup and (text) command endpoints return
  `409` for VM servers via `RequireFilesystem` / `RequireTextConsole`.
- **Networking** — port-forward rules are torn down for the previous guest IP on
  a DHCP lease change.

Residual limitations (acceptable for v1, tracked above):

- `ExitState` cannot distinguish a guest crash from a clean shutdown without the
  guest agent, so crash auto-restart is effectively a no-op for VMs.
- Port-forward application is asynchronous post-boot; failures are logged, not
  surfaced to the API caller.
- No global cap on concurrent `virsh` invocations (libvirt handles concurrency;
  revisit at high VM density).
- The console-attached state is not separately reported, so a command sent in
  the brief window before the serial console attaches may error.

## Trying it (bring-up checklist)

1. On a KVM host with libvirt + the tools above, build Wings from this branch.
2. Set `qemu.enabled: true` and a valid `bridge` in `config.yml`.
3. Put a cloud-init-ready qcow2 (e.g. an Ubuntu cloud image) in
   `template_directory`, or use an `http(s)://` URL as the server's image.
4. Create a server the panel marks `environment_type: "qemu"` (until the panel
   supports it, this can be set in the server's config for testing) with
   `vm.os=linux` and sensible CPU/RAM/disk limits.
5. Install (provisions the disk + domain), then Start. Watch the serial console;
   `virsh list`, `virsh domifaddr <uuid>` and `virsh domstats <uuid>` on the host
   help debug.
