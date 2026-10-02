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

const version = "0.4.0"

func main() {
	cfg := config.Load()
	l := logx.New(filepath.Join(config.Dir(), "logs"))
	l.SetTraffic(cfg.Get().LogTraffic)
	l.Log(logx.Info, "app", "FlexTCI %s starting; settings in %s", version, config.Dir())

	app := NewApp(l, cfg)
	err := wails.Run(&options.App{
		Title:     "FlexTCI " + version,
		Width:     330,
		Height:    820,
		MinWidth:  300,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 236, G: 236, B: 236, A: 1},
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
