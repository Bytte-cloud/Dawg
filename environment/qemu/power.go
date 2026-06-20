package qemu

import (
	"context"
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/environment"
)

// OnBeforeStart ensures the VM is provisioned and defined before it is booted.
// It must not take opMu — it is only ever called from Start, which already holds
// it.
func (e *Environment) OnBeforeStart(ctx context.Context) error {
	if err := e.enabled(); err != nil {
		return err
	}
	return e.Create()
}

// Start boots the virtual machine. Power operations are serialized via opMu so
// Start/Stop/Terminate/Destroy cannot interleave.
func (e *Environment) Start(ctx context.Context) error {
	if err := e.enabled(); err != nil {
		return err
	}

	e.opMu.Lock()
	defer e.opMu.Unlock()

	sawError := false
	// On any failure, walk the state machine down through "stopping" so the
	// crash detector does not treat the failed start as an unexpected exit.
	defer func() {
		if sawError {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
	}()

	if running, err := e.IsRunning(ctx); err != nil {
		return errors.Wrap(err, "qemu: failed to determine VM state before start")
	} else if running {
		e.SetState(environment.ProcessRunningState)
		return nil
	}

	e.SetState(environment.ProcessStartingState)

	if err := e.OnBeforeStart(ctx); err != nil {
		sawError = true
		return errors.WrapIf(err, "qemu: failed to prepare VM for boot")
	}

	if err := e.driver.Start(ctx, e.domainName()); err != nil {
		sawError = true
		return errors.WrapIf(err, "qemu: failed to start VM")
	}

	e.mu.Lock()
	e.startedAt = time.Now()
	e.exitCode = 0
	e.oomKilled = false
	e.mu.Unlock()

	// Create the per-run context that scopes the background goroutines.
	pctx := e.newProcContext()

	e.SetState(environment.ProcessRunningState)

	// Attach to the serial console after boot (the socket is created by libvirt
	// when the domain starts). A failure here is non-fatal — the VM is running.
	if err := e.Attach(ctx); err != nil {
		e.log().WithField("error", err).Warn("VM started but console attach failed")
	}

	// Port-forwards depend on the guest acquiring a DHCP lease, which happens
	// asynchronously after boot.
	go e.waitAndApplyPortForwards(pctx)

	return nil
}

// Stop requests a graceful ACPI shutdown of the guest.
func (e *Environment) Stop(ctx context.Context) error {
	if err := e.enabled(); err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.stop(ctx)
}

// stop performs the shutdown without taking opMu (callers must hold it).
func (e *Environment) stop(ctx context.Context) error {
	running, err := e.IsRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		e.SetState(environment.ProcessOfflineState)
		return nil
	}

	e.SetState(environment.ProcessStoppingState)
	if err := e.driver.Shutdown(ctx, e.domainName()); err != nil {
		if isNotFound(err) {
			e.SetState(environment.ProcessOfflineState)
			return nil
		}
		return errors.WrapIf(err, "qemu: failed to signal VM shutdown")
	}
	return nil
}

// WaitForStop initiates a shutdown and waits for the VM to power off. If it does
// not stop within the supplied duration it is either force-terminated
// (terminate == true) or an error is returned.
func (e *Environment) WaitForStop(ctx context.Context, duration time.Duration, terminate bool) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	if err := e.stop(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-tctx.Done():
			if terminate {
				return e.terminate(ctx)
			}
			return errors.WrapIf(tctx.Err(), "qemu: VM did not stop within the allotted time")
		case <-ticker.C:
			running, err := e.IsRunning(ctx)
			if err != nil {
				return err
			}
			if !running {
				e.SetState(environment.ProcessOfflineState)
				return nil
			}
		}
	}
}

// Terminate forcibly powers off the VM (equivalent to pulling the power cord).
// The signal argument is accepted for interface compatibility but a VM is always
// hard powered-off.
func (e *Environment) Terminate(ctx context.Context, signal string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.terminate(ctx)
}

// terminate performs the force power-off without taking opMu (callers must hold
// it).
func (e *Environment) terminate(ctx context.Context) error {
	running, err := e.IsRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		e.SetState(environment.ProcessOfflineState)
		return nil
	}

	e.SetState(environment.ProcessStoppingState)
	if err := e.driver.Destroy(ctx, e.domainName()); err != nil && !isNotFound(err) {
		return errors.WrapIf(err, "qemu: failed to terminate VM")
	}
	_ = e.removePortForwards(context.Background())
	e.SetState(environment.ProcessOfflineState)
	return nil
}

// waitAndApplyPortForwards polls for the guest's DHCP address and installs the
// allocation port-forwards once it is available. It exits promptly if the run
// context is cancelled (VM stopped) or the VM is no longer running.
func (e *Environment) waitAndApplyPortForwards(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if running, _ := e.IsRunning(ctx); !running {
			return
		}
		if ip, _ := e.guestIP(ctx); ip != "" {
			if err := e.applyPortForwards(ctx, ip); err != nil {
				e.log().WithField("error", err).Error("failed to apply VM port forwards; the VM may be unreachable")
			}
			return
		}
	}
	e.log().Warn("gave up waiting for VM to acquire an IP address for port forwarding")
}
