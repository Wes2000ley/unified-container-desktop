//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/controller"
	"github.com/skyri/ucd/internal/dockerproxy"
	"github.com/skyri/ucd/internal/engine"
	"github.com/skyri/ucd/internal/helper"
	"github.com/skyri/ucd/internal/local"
	"github.com/skyri/ucd/internal/pki"
	"github.com/skyri/ucd/internal/platform"
	"github.com/skyri/ucd/internal/proxyserver"
	"github.com/skyri/ucd/internal/resourcesaver"
	"github.com/skyri/ucd/internal/state"
)

func main() {
	root := flag.String("data-root", filepath.Join(os.Getenv("ProgramData"), "UnifiedContainerDesktop"), "machine-wide data directory")
	version := flag.Bool("version", false, "print the application version")
	flag.Parse()
	if *version {
		fmt.Println(api.Version)
		return
	}
	if err := local.RunService(func(ctx context.Context) error { return run(ctx, *root) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type failedCaller struct{ err error }

func (f failedCaller) Call(context.Context, api.Request) (any, error) { return nil, f.err }

func run(ctx context.Context, root string) error {
	if err := engine.ConfigureServiceDockerEnvironment(root); err != nil {
		return err
	}
	store, err := state.Open(root)
	if err != nil {
		return err
	}
	if err = store.Update(func(*api.Settings) error { return nil }); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(root, "logs"), 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(root, "logs", "controller.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	events := &api.EventBus{}
	settings := store.Snapshot()
	win, err := engine.New("windows", settings.WindowsEndpoint, settings.DataRoot, events)
	if err != nil {
		return fmt.Errorf("initialize Windows engine client: %w", err)
	}
	defer win.Close()
	var linux api.Caller
	var linuxDocker http.Handler
	if err = pki.Ensure(filepath.Join(root, "certificates")); err != nil {
		linux = failedCaller{fmt.Errorf("initialize helper certificates: %w", err)}
		linuxDocker = unavailableDocker(fmt.Errorf("initialize helper certificates: %w", err))
		events.Publish(api.Event{Type: "linux.failed", Engine: "linux", Message: err.Error()})
		logger.Error("helper certificates unavailable", "error", err)
	} else {
		client, e := helper.NewClient(settings)
		if e != nil {
			linux = failedCaller{e}
			linuxDocker = unavailableDocker(e)
			events.Publish(api.Event{Type: "linux.failed", Engine: "linux", Message: e.Error()})
		} else {
			linux = client
			linuxDocker = client.DockerHandler()
			defer client.Close()
		}
	}
	manager, infraErr := platform.New(settings, events)
	var infrastructure api.Caller = independentInfrastructure{infraErr}
	if manager != nil {
		infrastructure = manager
		if err = store.Update(func(s *api.Settings) error { s.WSL = manager.WSLSettings(); return nil }); err != nil {
			_ = manager.Close()
			return fmt.Errorf("reconcile WSL settings: %w", err)
		}
	}
	if infraErr != nil {
		events.Publish(api.Event{Type: "wsl.failed", Engine: "linux", Message: infraErr.Error()})
		logger.Error("WSL infrastructure unavailable", "error", infraErr)
	}
	owned, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	var saver *resourcesaver.Gate
	if manager != nil {
		saver, err = resourcesaver.New(resourcesaver.Config{Enabled: settings.ResourceSaver.Enabled, IdleSeconds: settings.ResourceSaver.IdleSeconds}, linux, manager, events)
		if err != nil {
			_ = manager.Close()
			return fmt.Errorf("initialize Linux Resource Saver: %w", err)
		}
		linux = saver
		linuxDocker = resourceSaverDocker(saver, linuxDocker)
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			lastError := ""
			for {
				select {
				case <-owned.Done():
					return
				case <-ticker.C:
					check, stop := context.WithTimeout(owned, 30*time.Second)
					err := saver.Tick(check)
					stop()
					message := ""
					if err != nil {
						message = err.Error()
					}
					if message != lastError && owned.Err() == nil {
						if err != nil {
							logger.Error("Linux Resource Saver", "error", err)
							events.Publish(api.Event{Type: "resource_saver.failed", Engine: "linux", Message: message})
						} else {
							events.Publish(api.Event{Type: "resource_saver.restored", Engine: "linux", Message: "Resource Saver checks recovered"})
						}
						lastError = message
					}
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		_ = proxyserver.Run(owned, "127.0.0.1:7444", func() api.ProxySettings { return win.RuntimeConfiguration().DockerProxy }, func(err error) {
			if err != nil {
				logger.Error("Windows Docker proxy", "error", err)
				events.Publish(api.Event{Type: "proxy.failed", Engine: "windows", Message: err.Error()})
			} else {
				events.Publish(api.Event{Type: "proxy.restored", Engine: "windows", Message: "Docker proxy configuration is active"})
			}
		})
	}()
	workers.Add(1)
	go func() { defer workers.Done(); _ = win.Run(owned) }()
	if manager != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := manager.EnsureProvisioned(owned); err != nil {
				events.Publish(api.Event{Type: "wsl.failed", Engine: "linux", Message: err.Error()})
				logger.Error("Linux provisioning failed", "error", err)
				return
			}
			if err := manager.Run(owned); err != nil {
				events.Publish(api.Event{Type: "wsl.failed", Engine: "linux", Message: err.Error()})
				logger.Error("Linux supervision failed", "error", err)
			}
		}()
	}
	service := controller.New(store, win, linux, infrastructure, events)
	service.SetResourceSaver(saver)
	events.Publish(api.Event{Type: "controller.started", Message: "Machine-wide controller started"})
	logger.Info("controller started", "version", api.Version)
	servers := make(chan error, 2)
	go func() {
		servers <- local.ServeWithDocker(owned, service, local.NewEngineRouter(win.DockerPolicyHandler(local.NewWindowsDockerHandler()), linuxDocker))
	}()
	go func() { servers <- local.ServeDocker(owned, linuxDocker) }()
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastError := ""
		for {
			select {
			case <-owned.Done():
				return
			case <-ticker.C:
				err := local.SyncDockerAccess(owned)
				if owned.Err() != nil {
					return
				}
				message := ""
				if err != nil {
					message = err.Error()
				}
				if message != lastError {
					if err != nil {
						logger.Error("refresh operator access", "error", err)
						events.Publish(api.Event{Type: "access.failed", Message: message})
					} else {
						events.Publish(api.Event{Type: "access.restored", Message: "Direct Docker endpoints are restricted to the controller and Windows administrators"})
					}
					lastError = message
				}
			}
		}
	}()
	err = <-servers
	cancel()
	if otherErr := <-servers; err == nil {
		err = otherErr
	}
	workers.Wait()
	if manager != nil {
		if closeErr := manager.Close(); closeErr != nil {
			logger.Error("release service profile", "error", closeErr)
		}
	}
	logger.Info("controller stopped")
	return err
}

func resourceSaverDocker(gate *resourcesaver.Gate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, err := gate.Acquire(r.Context())
		if err != nil {
			dockerproxy.WriteError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

func unavailableDocker(err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dockerproxy.WriteError(w, http.StatusServiceUnavailable, "Linux Docker API is unavailable: "+err.Error())
	})
}

type independentInfrastructure struct{ linuxError error }

func (i independentInfrastructure) Call(ctx context.Context, req api.Request) (any, error) {
	if req.Method == "operator.list" || req.Method == "operator.add" || req.Method == "operator.remove" || (req.Engine == "windows" && (req.Method == "engine.start" || req.Method == "engine.stop")) {
		return platform.WindowsCall(ctx, req)
	}
	if i.linuxError != nil {
		return nil, fmt.Errorf("WSL infrastructure: %w", i.linuxError)
	}
	return nil, api.Fail("unavailable", "WSL infrastructure is unavailable")
}
