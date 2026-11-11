package main

import (
	"embed"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/va3mw/SliceTCI/internal/config"
	"github.com/va3mw/SliceTCI/internal/logx"
)

//go:embed all:frontend/dist
var assets embed.FS

const version = "0.6.2"

// waitPIDFlag is passed to a restarted SliceTCI (after install or update) so
// it waits for the previous instance to exit before starting.
const waitPIDFlag = "-wait-pid"

func main() {
	if len(os.Args) > 1 && os.Args[1] == elevatedInstallFlag {
		os.Exit(runElevatedInstall(os.Args[2:]))
	}
	for i, a := range os.Args {
		if a == waitPIDFlag && i+1 < len(os.Args) {
			if pid, err := strconv.Atoi(os.Args[i+1]); err == nil {
				waitForExit(pid, 15*time.Second)
			}
		}
	}
	cfg := config.Load()
	l := logx.New(filepath.Join(config.Dir(), "logs"))
	l.SetTraffic(cfg.Get().LogTraffic)
	l.Log(logx.Info, "app", "SliceTCI %s starting; settings in %s", version, config.Dir())

	app := NewApp(l, cfg)
	err := wails.Run(&options.App{
		Title:     "SliceTCI " + version,
		Width:     330,
		Height:    820,
		MinWidth:  300,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 236, G: 236, B: 236, A: 1},
		StartHidden:      true, // shown by OnDomReady at its saved position
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnBeforeClose:    app.beforeClose,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "va3mw-slicetci-1",
		},
		Windows: &windows.Options{},
	})
	if err != nil {
		l.Log(logx.Error, "app", "wails: %v", err)
	}
}
