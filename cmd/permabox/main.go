//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/yijiangchengming/permabox/internal/config"
	"github.com/yijiangchengming/permabox/internal/lifecycle"
	"github.com/yijiangchengming/permabox/internal/mount"
	"github.com/yijiangchengming/permabox/internal/workload"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "validate-containerd-directory" {
		if err := validateContainerdDirectory(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "validate-installed-runtime" {
		if err := validateInstalledRuntime(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 4 {
		switch os.Args[1] {
		case "prepare-bridge-state":
			if err := prepareBridgeState(os.Args[2], os.Args[3]); err != nil {
				log.Fatal(err)
			}
			return
		case "check-bridge-marker":
			if err := checkBridgeMarker(os.Args[2], os.Args[3]); err != nil {
				os.Exit(1)
			}
			return
		case "validate-host-executable":
			if err := printHostExecutable(os.Args[2], os.Args[3]); err != nil {
				log.Fatal(err)
			}
			return
		case "validate-host-runtimes":
			if err := validateHostRuntimes(os.Args[2], os.Args[3]); err != nil {
				log.Fatal(err)
			}
			return
		case "prepare-runtime-cache", "verify-runtime-cache":
			if err := verifyRuntimeCache(os.Args[2], os.Args[3], os.Args[1] == "prepare-runtime-cache"); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	if len(os.Args) == 3 && os.Args[1] == "prepare-installer-state" {
		if err := prepareInstallerState(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
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
		if len(os.Args) == 2 && os.Args[1] == "validate-config" {
			fmt.Fprintln(os.Stderr, err)
		}
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
	report := func(format string, a ...any) { logError(cfg.LogFile, format, a...) }
	os.Exit(lifecycle.Run(cfg, mount.New(cfg.StateDir), os.Args[1:], report))
}

func logError(path, format string, args ...any) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil && info.Mode().IsRegular() {
		log.New(f, "permabox: ", log.LstdFlags|log.LUTC).Printf(format, args...)
	}
}
