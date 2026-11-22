//go:build linux

package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/config"
	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/ownership"
	"github.com/yijiangchengming/permabox/internal/workload"
)

// Run dispatches GC and OCI commands, returning their process exit status.
func Run(cfg config.Config, manager mount.Manager, args []string, report func(string, ...any)) int {
	routing := routes(manager.Dir)
	if len(args) == 1 && args[0] == "gc" {
		return collect(cfg, manager, routing, report)
	}
	inv, err := oci.Parse(args)
	if err != nil {
		report("%v", err)
		return 1
	}
	runtime := cfg.Runtime
	var pinned route
	if inv.ID != "" {
		pinned, err = routing.lookup(inv.ID)
		if err != nil {
			report("%s %s: read runtime choice: %v", inv.Command, inv.ID, err)
			return 1
		}
		if pinned.Runtime != "" {
			runtime = pinned.Runtime
		}
	}
	var installed, pinnedNow bool
	var executable string
	switch inv.Command {
	case "create":
		options, err := oci.InspectBundle(inv.Bundle, cfg.Annotation)
		if err != nil {
			report("create %s: inspect bundle: %v", inv.ID, err)
			return 1
		}
		{
			if bundle := options.Persistence; bundle != nil && !cfg.TrustsNamespace(bundle.Namespace) {
				report("create %s: namespace %q is not trusted for persistence; check trusted_namespaces", inv.ID, bundle.Namespace)
				return 1
			}
			if options.RuntimeAlias != "" {
				var ok bool
				runtime, ok = cfg.UnderlyingRuntimes[options.RuntimeAlias]
				if !ok {
					report("create %s: unknown runtime alias %q", inv.ID, options.RuntimeAlias)
					return 1
				}
			}
			storage := cfg.StorageRoot
			if options.StorageAlias != "" {
				var ok bool
				storage, ok = cfg.StorageRoots[options.StorageAlias]
				if !ok {
					report("create %s: unknown storage alias %q", inv.ID, options.StorageAlias)
					return 1
				}
			}
			executable, err = checkRuntime(runtime)
			if err != nil {
				report("%v", err)
				return 1
			}
			pinnedNow, err = routing.pin(inv.ID, runtime, inv.Bundle)
			if err != nil {
				report("create %s: pin runtime: %v", inv.ID, err)
				return 1
			}
			if bundle := options.Persistence; bundle != nil {
				slot := filepath.Join(storage, bundle.Namespace, bundle.Pod)
				owner, lookupErr := workloadClient(cfg).Resolve(context.Background(), bundle.Namespace, bundle.Pod, bundle.PodUID)
				if lookupErr != nil || !owner.Valid() {
					if pinnedNow {
						lookupErr = errors.Join(lookupErr, routing.forget(inv.ID))
					}
					if lookupErr == nil {
						lookupErr = errors.New("persistence requires a verified StatefulSet owner")
					}
					report("create %s: %v", inv.ID, lookupErr)
					return 1
				}
				installed, err = manager.PrepareOwned(inv.ID, bundle.Root, slot, bundle.Container, owner)
				if err != nil {
					managed, inspectErr := manager.Managed(inv.ID)
					if pinnedNow && !managed && inspectErr == nil {
						err = errors.Join(err, routing.forget(inv.ID))
					}
					report("create %s: %v", inv.ID, err)
					return 1
				}
			}
		}
	case "delete":
		managed, err := manager.Managed(inv.ID)
		if err != nil {
			report("delete %s: inspect state: %v", inv.ID, err)
			return 1
		}
		if managed || pinned.Runtime != "" {
			executable, err := checkRuntime(runtime)
			if err != nil {
				report("%v", err)
				return 1
			}
			if code := runRuntime(runtime, executable, args, inv, report); code != 0 {
				return code
			}
			if managed {
				if err := manager.Cleanup(inv.ID); err != nil {
					report("delete %s: %v", inv.ID, err)
					return 1
				}
			}
			if pinned.Runtime != "" {
				if err := routing.forget(inv.ID); err != nil {
					report("delete %s: %v", inv.ID, err)
					return 1
				}
			}
			return 0
		}
	}
	if executable == "" {
		executable, err = checkRuntime(runtime)
		if err != nil {
			report("%v", err)
			return 1
		}
	}
	if inv.Command == "start" {
		managed, err := manager.Managed(inv.ID)
		if err != nil {
			report("start %s: inspect state: %v", inv.ID, err)
			return 1
		}
		if managed {
			if code := runRuntime(runtime, executable, args, inv, report); code != 0 {
				return code
			}
			if err := manager.ActivateOwnership(inv.ID); err != nil {
				report("start %s: commit workload ownership: %v", inv.ID, err)
			}
			return 0
		}
	}
	argv := append([]string{runtime}, args...)
	if err := syscall.Exec(executable, argv, os.Environ()); err != nil {
		report("exec %s: %v", runtime, err)
		if installed {
			if err := manager.Cleanup(inv.ID); err != nil {
				report("create %s: rollback: %v", inv.ID, err)
				return 1
			}
		}
		if pinnedNow {
			if err := routing.forget(inv.ID); err != nil {
				report("create %s: forget runtime choice: %v", inv.ID, err)
			}
		}
		return 1
	}
	return 0
}

func runRuntime(runtime, executable string, args []string, inv oci.Invocation, report func(string, ...any)) int {
	cmd := exec.Command(runtime, args...)
	cmd.Path = executable
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if status, ok := err.(*exec.ExitError); ok {
			if wait, ok := status.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
				return 128 + int(wait.Signal())
			}
			return status.ExitCode()
		}
		report("%s %s: launch runtime: %v", inv.Command, inv.ID, err)
		return 1
	}
	return 0
}

func collect(cfg config.Config, manager mount.Manager, routing routes, report func(string, ...any)) int {
	now := time.Now()
	roots := cfg.AllStorageRoots()
	var sweep error
	if cfg.WorkloadGC() {
		sweep = sweepWorkloads(manager, roots, "", cfg.GCIdleTTL, now, workloadReclaimer(cfg))
	} else {
		sweep = manager.Sweep(roots, cfg.GCIdleTTL, now)
	}
	if err := errors.Join(sweep, routing.sweep(now, manager.Managed)); err != nil {
		report("gc: %v", err)
		return 1
	}
	return 0
}

func workloadReclaimer(cfg config.Config) reclaimer {
	client := workloadClient(cfg)
	return func(ctx context.Context, owner ownership.Owner) (reclaimDecision, error) {
		onDelete, onZero := cfg.GCOnWorkloadDelete, cfg.GCOnScaleToZero
		if cfg.GCWorkloadAnnotations {
			onDelete, onZero = workload.ApplyOverrides(owner.Overrides, onDelete, onZero)
		}
		reason, generation, err := client.Reclaim(ctx, owner, onDelete, onZero)
		if err != nil {
			return reclaimDecision{}, err
		}
		switch reason {
		case workload.ReclaimDeleted:
			return reclaimDecision{reason: mount.ReclaimDeleted}, nil
		case workload.ReclaimZero:
			return reclaimDecision{reason: mount.ReclaimZero, generation: generation}, nil
		}
		return reclaimDecision{}, nil
	}
}

func workloadClient(cfg config.Config) workload.Client {
	path := cfg.WorkloadSocket
	if path == "" {
		path = workload.DefaultSocket
	}
	return workload.UnixClient(path)
}
