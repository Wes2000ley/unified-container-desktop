//go:build windows

package main

import (
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync/atomic"

	"github.com/skyri/ucd/frontend"
	"github.com/skyri/ucd/internal/local"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Never redirect the desktop UI to an external development server.
	if err := os.Unsetenv("FRONTEND_DEVSERVER_URL"); err != nil {
		fmt.Fprintln(os.Stderr, "clear development server setting:", err)
		return 1
	}
	assets, err := fs.Sub(frontend.Assets, "dist")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load desktop assets:", err)
		return 1
	}
	preferences, err := newDesktopPreferencesStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load desktop preferences:", err)
		return 1
	}
	client := local.NewClient()
	defer client.Close()
	var exitRequested atomic.Bool
	app := application.New(application.Options{
		Name:        "Unified Container Desktop",
		Description: "Manage the machine's Linux and Windows containers",
		Icon:        ucdIconPNG,
		OnShutdown: func() {
			exitRequested.Store(true)
		},
		Assets:   application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Services: []application.Service{application.NewService(&Bridge{client: client, preferences: preferences})},
	})
	startHidden := slices.Contains(os.Args[1:], "--background") && preferences.Get().StartMinimized
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "Unified Container Desktop",
		URL:              "/",
		Width:            1320,
		Height:           860,
		MinWidth:         1000,
		MinHeight:        680,
		Hidden:           startHidden,
		BackgroundColour: application.NewRGBA(17, 22, 29, 255),
	})
	tray := app.SystemTray.New()
	tray.SetIcon(ucdIconPNG).SetTooltip("Unified Container Desktop")
	menu := application.NewMenu()
	menu.Add("Open Unified Container Desktop").OnClick(func(*application.Context) {
		showDesktopWindow(window)
	})
	menu.AddSeparator()
	menu.Add("Exit").OnClick(func(*application.Context) {
		exitRequested.Store(true)
		app.Quit()
	})
	tray.SetMenu(menu).OnClick(func() {
		showDesktopWindow(window)
	})
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if exitRequested.Load() {
			return
		}
		if preferences.Get().CloseToTray {
			event.Cancel()
			window.Hide()
		}
	})
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "run desktop:", err)
		return 1
	}
	return 0
}

func showDesktopWindow(window *application.WebviewWindow) {
	window.Show()
	if window.IsMinimised() {
		window.Restore()
	}
	window.Focus()
}
