package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
	"gopkg.in/yaml.v3"

	"github.com/pterodactyl/wings/config"
)

// buildSeedISO generates a cloud-init NoCloud datasource ISO for Linux guests.
// It derives credentials and SSH keys from the server's environment variables
// (VM_USER, VM_PASSWORD, VM_SSH_KEYS, VM_HOSTNAME) and writes the resulting
// seed.iso into the VM data directory. The path to the ISO is returned, or an
// empty string when seeding is not applicable (non-Linux guests).
//
// To avoid ever logging or persisting a generated secret, provisioning requires
// the operator to supply VM_PASSWORD and/or VM_SSH_KEYS; nothing is auto-generated.
func (e *Environment) buildSeedISO(ctx context.Context) (string, error) {
	if e.meta.guestOS() != OSLinux {
		return "", nil
	}

	env := envMap(e.Config().EnvironmentVariables())

	hostname := sanitizeHostname(firstNonEmpty(env["VM_HOSTNAME"], "vps-"+shortID(e.Id)))
	username := sanitizeUsername(firstNonEmpty(env["VM_USER"], "root"))

	var sshKeys []string
	if raw := strings.TrimSpace(env["VM_SSH_KEYS"]); raw != "" {
		for _, k := range strings.Split(raw, "\n") {
			for _, k2 := range strings.Split(k, ",") {
				if v := strings.TrimSpace(k2); v != "" {
					sshKeys = append(sshKeys, v)
				}
			}
		}
	}

	password := env["VM_PASSWORD"]
	if password == "" && len(sshKeys) == 0 {
		return "", errors.New("qemu: VM provisioning requires VM_PASSWORD and/or VM_SSH_KEYS to be set for the initial login")
	}

	user := map[string]interface{}{
		"name":        username,
		"sudo":        "ALL=(ALL) NOPASSWD:ALL",
		"groups":      "sudo",
		"shell":       "/bin/bash",
		"lock_passwd": password == "", // disable password login when only keys are provided
	}
	if len(sshKeys) > 0 {
		user["ssh_authorized_keys"] = sshKeys
	}

	cloudConfig := map[string]interface{}{
		"hostname":         hostname,
		"manage_etc_hosts": true,
		"ssh_pwauth":       password != "",
		"users":            []interface{}{user},
		// Grow the root partition/filesystem to fill the resized disk.
		"growpart":      map[string]interface{}{"mode": "auto", "devices": []string{"/"}},
		"resize_rootfs": true,
	}
	if password != "" {
		cloudConfig["chpasswd"] = map[string]interface{}{
			"expire": false,
			"users": []interface{}{
				map[string]interface{}{"name": username, "password": password, "type": "text"},
			},
		}
	}

	body, err := yaml.Marshal(cloudConfig)
	if err != nil {
		return "", errors.Wrap(err, "qemu: failed to marshal cloud-config")
	}
	userData := append([]byte("#cloud-config\n"), body...)

	// meta-data marshalled via YAML so values (e.g. hostname) cannot inject
	// additional document structure.
	metaData, err := yaml.Marshal(map[string]interface{}{
		"instance-id":    e.Id,
		"local-hostname": hostname,
	})
	if err != nil {
		return "", errors.Wrap(err, "qemu: failed to marshal meta-data")
	}

	ns := config.Get().Qemu.Nameserver
	if net.ParseIP(ns) == nil {
		ns = "1.1.1.1"
	}
	networkData, err := yaml.Marshal(map[string]interface{}{
		"version": 2,
		"ethernets": map[string]interface{}{
			"eth0": map[string]interface{}{
				"match":       map[string]interface{}{"name": "e*"},
				"dhcp4":       true,
				"nameservers": map[string]interface{}{"addresses": []string{ns}},
			},
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "qemu: failed to marshal network-config")
	}

	seedDir := filepath.Join(e.dataDir(), "cloudinit")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return "", errors.Wrap(err, "qemu: failed to create cloud-init directory")
	}
	for name, data := range map[string][]byte{"user-data": userData, "meta-data": metaData, "network-config": networkData} {
		if err := os.WriteFile(filepath.Join(seedDir, name), data, 0o600); err != nil {
			return "", errors.Wrapf(err, "qemu: failed to write %s", name)
		}
	}

	if err := e.makeISO(ctx, e.seedISOPath(), seedDir); err != nil {
		return "", err
	}

	return e.seedISOPath(), nil
}

// makeISO builds an ISO9660 image labelled "cidata" from the files in srcDir.
// It tries the common mkisofs-compatible tools in order of availability and
// reports every failure if none succeed. Any pre-existing ISO is removed first
// so the operation is safely retryable.
func (e *Environment) makeISO(ctx context.Context, isoPath, srcDir string) error {
	_ = os.Remove(isoPath)

	userData := filepath.Join(srcDir, "user-data")
	metaData := filepath.Join(srcDir, "meta-data")
	networkConfig := filepath.Join(srcDir, "network-config")

	candidates := [][]string{
		{"genisoimage", "-output", isoPath, "-volid", "cidata", "-joliet", "-rock", userData, metaData, networkConfig},
		{"mkisofs", "-output", isoPath, "-volid", "cidata", "-joliet", "-rock", userData, metaData, networkConfig},
		{"xorriso", "-as", "mkisofs", "-o", isoPath, "-V", "cidata", "-J", "-r", userData, metaData, networkConfig},
	}

	var errs []string
	for _, c := range candidates {
		if _, err := e.driver.run(ctx, c[0], c[1:]...); err == nil {
			return nil
		} else {
			errs = append(errs, fmt.Sprintf("%s: %v", c[0], err))
		}
	}
	return errors.Errorf("qemu: failed to build cloud-init ISO (install genisoimage, mkisofs or xorriso): %s", strings.Join(errs, "; "))
}

func envMap(vars []string) map[string]string {
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		if i := strings.IndexByte(v, '='); i > 0 {
			out[v[:i]] = v[i+1:]
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// sanitizeHostname returns an RFC1123-ish hostname: lowercase alphanumerics and
// hyphens, not starting/ending with a hyphen, max 63 chars.
func sanitizeHostname(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" {
		out = "vps"
	}
	return out
}

// sanitizeUsername restricts the login name to a safe POSIX set.
func sanitizeUsername(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "root"
	}
	return out
}
