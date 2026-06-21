package qemu

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/config"
)

// maxTemplateBytes caps the size of a downloaded template image to avoid a
// malicious or misconfigured URL exhausting host disk.
const maxTemplateBytes int64 = 64 << 30 // 64 GiB

// ensureBaseImage resolves the configured template image to a concrete path on
// disk, downloading it into the template directory if it is given as an http(s)
// URL and is not already cached. Supported forms for Metadata.Image:
//
//	/absolute/path/to/template.qcow2   -> used directly (admin-controlled)
//	template.qcow2                      -> resolved under the template directory
//	https://host/path/template.qcow2    -> downloaded into the template directory
func (e *Environment) ensureBaseImage(ctx context.Context) (string, error) {
	image := strings.TrimSpace(e.meta.Image)
	if image == "" {
		return "", errors.New("qemu: no template image configured for VM (set the server's container image to a qcow2 template path or URL)")
	}

	tmplDir := config.Get().Qemu.TemplateDirectory
	if err := os.MkdirAll(tmplDir, 0o700); err != nil {
		return "", errors.Wrap(err, "qemu: failed to create template directory")
	}

	// Absolute local path — used directly (this is set by the panel egg/admin,
	// not by end users).
	if filepath.IsAbs(image) && !isURL(image) {
		if _, err := os.Stat(image); err != nil {
			return "", errors.Wrapf(err, "qemu: template image %s is not accessible", image)
		}
		return image, nil
	}

	// Remote URL — download into the template dir keyed by the URL's filename.
	if isURL(image) {
		name := sanitizeBase(filepath.Base(image))
		if name == "" {
			name = "template-" + sanitizeFilename(image) + ".qcow2"
		}
		dest := filepath.Join(tmplDir, name)
		if _, err := os.Stat(dest); err == nil {
			return dest, nil // already cached
		}
		if err := downloadFile(ctx, image, dest); err != nil {
			return "", err
		}
		return dest, nil
	}

	// Bare (relative) filename — resolve under the template directory and ensure
	// the result cannot escape it via "..".
	clean := filepath.Clean(filepath.Join(tmplDir, image))
	if clean != tmplDir && !strings.HasPrefix(clean, tmplDir+string(os.PathSeparator)) {
		return "", errors.Errorf("qemu: template image %q resolves outside the template directory", image)
	}
	if _, err := os.Stat(clean); err != nil {
		return "", errors.Wrapf(err, "qemu: template image %s not found in template directory", image)
	}
	return clean, nil
}

// provisionDisk creates the per-VM copy-on-write disk overlay from the resolved
// base image, then grows it to the panel-configured size. It is a no-op if the
// disk already exists.
func (e *Environment) provisionDisk(ctx context.Context) error {
	if err := os.MkdirAll(e.dataDir(), 0o700); err != nil {
		return errors.Wrap(err, "qemu: failed to create VM data directory")
	}

	if _, err := os.Stat(e.diskPath()); err == nil {
		return nil // disk already provisioned
	}

	base, err := e.ensureBaseImage(ctx)
	if err != nil {
		return err
	}

	if err := e.driver.CreateOverlay(ctx, base, e.diskPath()); err != nil {
		return errors.Wrap(err, "qemu: failed to create disk overlay")
	}

	// Grow the overlay to the requested size, but never shrink it: qemu-img
	// refuses to shrink (it would discard data past the new end), and a CoW
	// overlay can't be smaller than its backing image anyway. If the panel disk
	// limit is below the base image's virtual size we leave it at the image size.
	// (The guest filesystem is expanded inside the VM by cloud-init's
	// growpart/resizefs for Linux templates.)
	target := diskBytes(e.Config().Limits())
	current, err := e.driver.VirtualSize(ctx, e.diskPath())
	if err != nil {
		return errors.Wrap(err, "qemu: failed to read disk overlay size")
	}
	if target > current {
		if err := e.driver.Resize(ctx, e.diskPath(), diskSizeString(e.Config().Limits())); err != nil {
			return errors.Wrap(err, "qemu: failed to resize disk overlay")
		}
	}

	return nil
}

// removeStorage deletes all on-disk artifacts for the VM (disk, nvram, seed,
// console socket and the containing directory).
func (e *Environment) removeStorage() error {
	return os.RemoveAll(e.dataDir())
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// sanitizeBase strips any path separators / parent references from a filename so
// it cannot escape the template directory.
func sanitizeBase(name string) string {
	name = strings.TrimSpace(name)
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return name
}

func sanitizeFilename(s string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "?", "_", "&", "_", "=", "_", "\\", "_")
	return r.Replace(s)
}

// downloadFile streams a remote file to dest via a temporary file that is
// atomically renamed on success. The transfer is bounded by maxTemplateBytes and
// uses a response-header timeout to avoid hanging indefinitely on a dead server.
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.Wrap(err, "qemu: failed to build download request")
	}

	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		},
	}

	res, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "qemu: failed to download template image")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.Errorf("qemu: template download returned unexpected status %d", res.StatusCode)
	}
	if res.ContentLength > maxTemplateBytes {
		return errors.Errorf("qemu: template image is too large (%d bytes, limit %d)", res.ContentLength, maxTemplateBytes)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".dl-*")
	if err != nil {
		return errors.Wrap(err, "qemu: failed to create temporary download file")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// LimitReader guards against a server that lies about (or omits)
	// Content-Length; one extra byte lets us detect an overflow.
	written, err := io.Copy(tmp, io.LimitReader(res.Body, maxTemplateBytes+1))
	if err != nil {
		tmp.Close()
		return errors.Wrap(err, "qemu: failed while writing template image")
	}
	if written > maxTemplateBytes {
		tmp.Close()
		return errors.Errorf("qemu: template image exceeded the %d byte limit", maxTemplateBytes)
	}
	if err := tmp.Close(); err != nil {
		return errors.Wrap(err, "qemu: failed to flush template image")
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return errors.Wrap(err, "qemu: failed to finalize template image")
	}
	return nil
}
