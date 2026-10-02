// audiotest lists audio endpoints and briefly opens the DAX devices, to check
// the WASAPI layer on a given PC without the radio. It never keys the radio.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/va3mw/FlexTCI/internal/audio"
	"github.com/va3mw/FlexTCI/internal/logx"
)

func main() {
	dax := flag.Int("dax", 1, "DAX RX channel to capture from")
	rxName := flag.String("rx", "", "capture device name (default: auto DAX Audio RX n)")
	txName := flag.String("tx", "", "render device name (default: auto DAX Audio TX)")
	flag.Parse()
	l := logx.New(os.TempDir())
	l.OnEntry(func(e logx.Entry) { fmt.Printf("%s %-5s %s\n", e.Time, e.Level, e.Msg) })
	src := l.Src("audiotest")

	for _, capture := range []bool{true, false} {
		devs, err := audio.ListDevices(capture)
		kind := map[bool]string{true: "capture", false: "render"}[capture]
		if err != nil {
			fmt.Println(kind, "error:", err)
			continue
		}
		fmt.Printf("%d %s devices:\n", len(devs), kind)
		for _, d := range devs {
			fmt.Println("  ", d.Name)
		}
	}

	var mu sync.Mutex
	frames, peak := 0, 0.0
	capt := audio.StartCapture(src, *rxName, audio.AutoRXPattern(*dax), func(s []float32) {
		mu.Lock()
		frames += len(s) / 2
		for _, v := range s {
			peak = math.Max(peak, math.Abs(float64(v)))
		}
		mu.Unlock()
	})
	time.Sleep(time.Second)
	capt.Stop()
	fmt.Printf("capture: %d frames in 1 s (expect ~48000), peak %.3f\n", frames, peak)

	pulled := 0
	ren := audio.StartRender(src, *txName, audio.AutoTX, func(out []float32) {
		for i := range out {
			out[i] = 0
		}
		mu.Lock()
		pulled += len(out) / 2
		mu.Unlock()
	})
	time.Sleep(500 * time.Millisecond)
	ren.Stop()
	fmt.Printf("render: device pulled %d frames of silence in 0.5 s (expect ~24000 + buffer)\n", pulled)
}
