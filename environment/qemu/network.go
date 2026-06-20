package qemu

import (
	"context"
	"strconv"
	"strings"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/environment"
)

// guestIP discovers the VM's primary IPv4 address using `virsh domifaddr`
// (lease database, falling back to the guest agent). An empty string with no
// error is returned when no address could be determined yet (the guest may
// still be booting / acquiring DHCP).
func (e *Environment) guestIP(ctx context.Context) (string, error) {
	out, err := e.driver.GuestInterfaceAddresses(ctx, e.domainName())
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for _, f := range fields {
			// Match an "ip/mask" token and ensure it is IPv4.
			if strings.Contains(f, "/") && strings.Count(f, ".") == 3 {
				ip := strings.SplitN(f, "/", 2)[0]
				if ip != "" && ip != "127.0.0.1" {
					return ip, nil
				}
			}
		}
	}
	return "", nil
}

// portMapping is a single host port that should be forwarded into the guest.
type portMapping struct {
	HostIP string
	Port   int
}

// collectMappings flattens the panel allocation structure into a list of host
// ports to forward.
func collectMappings(a environment.Allocations) []portMapping {
	var out []portMapping
	if a.DefaultMapping.Port != 0 {
		out = append(out, portMapping{HostIP: a.DefaultMapping.Ip, Port: a.DefaultMapping.Port})
	}
	for ip, ports := range a.Mappings {
		for _, p := range ports {
			// Avoid duplicating the default mapping.
			if ip == a.DefaultMapping.Ip && p == a.DefaultMapping.Port {
				continue
			}
			out = append(out, portMapping{HostIP: ip, Port: p})
		}
	}
	return out
}

// applyPortForwards installs iptables DNAT + FORWARD rules so that the host
// allocations route to the guest at the supplied address. Rules previously
// installed for a different guest address are removed first so a DHCP lease
// change does not leave stale rules behind.
func (e *Environment) applyPortForwards(ctx context.Context, guest string) error {
	if guest == "" {
		return nil
	}

	e.mu.Lock()
	old := e.lastGuestIP
	e.mu.Unlock()
	if old != "" && old != guest {
		e.removeRulesFor(ctx, old)
	}

	for _, m := range collectMappings(e.Config().Allocations()) {
		for _, proto := range []string{"tcp", "udp"} {
			if err := e.iptables(ctx, "-A", m, guest, proto); err != nil {
				return err
			}
		}
	}

	e.mu.Lock()
	e.lastGuestIP = guest
	e.mu.Unlock()
	return nil
}

// removePortForwards deletes the rules installed for the last-known guest
// address. It is best-effort and safe to call when none exist.
func (e *Environment) removePortForwards(ctx context.Context) error {
	e.mu.Lock()
	ip := e.lastGuestIP
	e.lastGuestIP = ""
	e.mu.Unlock()
	if ip == "" {
		return nil
	}
	e.removeRulesFor(ctx, ip)
	return nil
}

// removeRulesFor deletes the DNAT/FORWARD rules for one guest address. Errors
// are ignored because a rule may simply not exist.
func (e *Environment) removeRulesFor(ctx context.Context, guest string) {
	for _, m := range collectMappings(e.Config().Allocations()) {
		for _, proto := range []string{"tcp", "udp"} {
			_ = e.iptables(ctx, "-D", m, guest, proto)
		}
	}
}

// iptables adds (-A) or deletes (-D) the DNAT and matching FORWARD rule for one
// host port -> guest mapping.
func (e *Environment) iptables(ctx context.Context, action string, m portMapping, guest, proto string) error {
	comment := "wings-vm-" + e.Id
	port := strconv.Itoa(m.Port)

	dnat := []string{"-t", "nat", action, "PREROUTING", "-p", proto, "--dport", port}
	if m.HostIP != "" && m.HostIP != "0.0.0.0" {
		dnat = append(dnat, "-d", m.HostIP)
	}
	dnat = append(dnat, "-m", "comment", "--comment", comment, "-j", "DNAT", "--to-destination", guest+":"+port)
	if _, err := e.driver.run(ctx, "iptables", dnat...); err != nil && action == "-A" {
		return errors.Wrap(err, "qemu: failed to apply DNAT rule")
	}

	fwd := []string{action, "FORWARD", "-p", proto, "-d", guest, "--dport", port, "-m", "comment", "--comment", comment, "-j", "ACCEPT"}
	if _, err := e.driver.run(ctx, "iptables", fwd...); err != nil && action == "-A" {
		return errors.Wrap(err, "qemu: failed to apply FORWARD rule")
	}
	return nil
}
