package main

import (
	"embed"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/va3mw/FlexTCI/internal/config"
	"github.com/va3mw/FlexTCI/internal/logx"
)

//go:embed all:frontend/dist
var assets embed.FS

const version = "0.2.1"

func main() {
	cfg := config.Load()
	l := logx.New(filepath.Join(config.Dir(), "logs"))
	l.SetTraffic(cfg.Get().LogTraffic)
	l.Log(logx.Info, "app", "FlexTCI %s starting; settings in %s", version, config.Dir())

	app := NewApp(l, cfg)
	err := wails.Run(&options.App{
		Title:     "FlexTCI " + version,
		Width:     1180,
		Height:    820,
		MinWidth:  900,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 31, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "va3mw-flextci-1",
		},
		Windows: &windows.Options{},
	})
	if err != nil {
		l.Log(logx.Error, "app", "wails: %v", err)
	}
}
