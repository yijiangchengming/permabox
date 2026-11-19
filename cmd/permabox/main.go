//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/yijiangchengming/permabox/internal/config"
	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/oci"
	"github.com/yijiangchengming/permabox/internal/workload"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "check-workload-bridge" {
		if err := workload.CheckSocket(os.Args[2]); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "serve-workloads" {
		if err := workload.Serve(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := config.Load()
	if err != nil {
		path := os.Getenv("PERMABOX_LOG_FILE")
		if path == "" {
			path = cfg.LogFile
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			path = config.DefaultLog
		}
		logError(path, "%v", err)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "validate-config" {
		return
	}
	os.Exit(run(cfg, mount.New(config.StateDir), os.Args[1:]))
}

func run(cfg config.Config, manager mount.Manager, args []string) int {
	report := func(format string, a ...any) { logError(cfg.LogFile, format, a...) }
	routing := routes(manager.Dir)
	if len(args) == 1 && args[0] == "gc" {
		return collect(cfg, manager, routing)
	}
	inv := oci.Parse(args)
	runtime := cfg.Runtime
	var pinned route
	var err error
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
	var installed, pinnedNow, validated bool
	switch inv.Command {
	case "create":
		options, err := oci.InspectBundle(inv.Bundle, cfg.Annotation)
		if err != nil {
			report("create %s: inspect bundle: %v", inv.ID, err)
			// Leave runtime-specific diagnostics and exit codes to the runtime
			// when its OCI config could not be read or decoded.
			if oci.IsInvalidPersistence(err) {
				return 1
			}
		} else {
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
			if err := checkRuntime(runtime); err != nil {
				report("%v", err)
				return 1
			}
			validated = true
			pinnedNow, err = routing.pin(inv.ID, runtime, inv.Bundle)
			if err != nil {
				report("create %s: pin runtime: %v", inv.ID, err)
				return 1
			}
			if bundle := options.Persistence; bundle != nil {
				installed, err = preparePersistence(cfg, manager, inv.ID, bundle, storage, runtime, report)
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
			if err := checkRuntime(runtime); err != nil {
				report("%v", err)
				return 1
			}
			cmd := exec.Command(runtime, args...)
			cmd.Env = os.Environ()
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				if status, ok := err.(*exec.ExitError); ok {
					if wait, ok := status.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
						return 128 + int(wait.Signal())
					}
					return status.ExitCode()
				}
				report("delete %s: launch runtime: %v", inv.ID, err)
				return 1
			}
			if managed {
				slot, err := manager.CleanupSlot(inv.ID)
				if err != nil {
					report("delete %s: %v", inv.ID, err)
					return 1
				}
				if cfg.WorkloadGC() {
					if err := manager.SweepReleasedWorkload(cfg.AllStorageRoots(), slot, workloadReclaimer(context.Background(), cfg)); err != nil {
						report("delete %s: workload GC will retry: %v", inv.ID, err)
					}
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
	if !validated {
		if err := checkRuntime(runtime); err != nil {
			report("%v", err)
			return 1
		}
	}
	if inv.Command == "start" {
		if err := manager.ActivateOwnership(inv.ID); err != nil {
			report("start %s: commit workload ownership: %v", inv.ID, err)
			return 1
		}
	}
	argv := append([]string{runtime}, args...)
	if err := syscall.Exec(runtime, argv, os.Environ()); err != nil {
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

func preparePersistence(cfg config.Config, manager mount.Manager, id string, bundle *oci.Bundle, storage, runtime string, report func(string, ...any)) (bool, error) {
	client := workloadClient(cfg)
	if bundle.ReusePolicy == "deployment-local" {
		if !cfg.DeploymentLocalReuse || !slices.Contains(cfg.DeploymentLocalReuseNamespaces, bundle.Namespace) {
			return false, fmt.Errorf("deployment-local reuse is disabled for namespace %q", bundle.Namespace)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		owner, err := client.ResolveDeployment(ctx, bundle.Namespace, bundle.Pod, bundle.PodUID)
		if err != nil {
			return false, fmt.Errorf("verify Deployment owner: %w", err)
		}
		image, err := oci.ContainerImageID(ctx, cfg.K3sBin, cfg.CRIEndpoint, id)
		if err != nil {
			report("create %s: no verified image identity; using a fresh Deployment layer: %v", id, err)
		}
		claim := mount.DeploymentClaim{Pod: bundle.Pod, PodUID: bundle.PodUID, Image: image, Runtime: runtime}
		return manager.PrepareDeployment(ctx, id, bundle.Root, storage, bundle.Container, owner, claim, cfg.GCIdleTTL,
			func(ctx context.Context, name, uid string) (bool, error) {
				return client.PodFinished(ctx, bundle.Namespace, name, uid)
			})
	}
	var owner workload.Owner
	if cfg.WorkloadGC() {
		var err error
		owner, err = client.Resolve(context.Background(), bundle.Namespace, bundle.Pod, bundle.PodUID)
		if err != nil {
			report("create %s: could not identify workload for GC: %v", id, err)
		}
	}
	return manager.PrepareOwned(id, bundle.Root, filepath.Join(storage, bundle.Namespace, bundle.Pod), bundle.Container, owner)
}

func collect(cfg config.Config, manager mount.Manager, routing routes) int {
	now := time.Now()
	roots := cfg.AllStorageRoots()
	var sweep error
	if cfg.WorkloadGC() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		sweep = manager.SweepWorkloads(roots, cfg.GCIdleTTL, now, workloadReclaimer(ctx, cfg))
	} else {
		sweep = manager.Sweep(roots, cfg.GCIdleTTL, now)
	}
	if err := errors.Join(sweep, routing.sweep(now, manager.Managed)); err != nil {
		logError(cfg.LogFile, "gc: %v", err)
		return 1
	}
	return 0
}

func workloadReclaimer(ctx context.Context, cfg config.Config) func(workload.Owner) (bool, error) {
	client := workloadClient(cfg)
	return func(owner workload.Owner) (bool, error) {
		onDelete, onZero := cfg.GCOnWorkloadDelete, cfg.GCOnScaleToZero
		if cfg.GCWorkloadAnnotations {
			onDelete, onZero = owner.Overrides.Apply(onDelete, onZero)
		}
		return client.Reclaim(ctx, owner, onDelete, onZero)
	}
}

func workloadClient(cfg config.Config) workload.Client {
	path := cfg.WorkloadSocket
	if path == "" {
		path = workload.DefaultSocket
	}
	return workload.UnixClient(path)
}

func checkRuntime(path string) error {
	target, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !target.Mode().IsRegular() || target.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("underlying runtime %s is not an executable file", path)
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	if os.SameFile(target, self) {
		return fmt.Errorf("underlying runtime %s points to PermaBox", path)
	}
	return nil
}

func logError(path, format string, args ...any) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil && info.Mode().IsRegular() {
		log.New(f, "permabox: ", log.LstdFlags|log.LUTC).Printf(format, args...)
	}
}
