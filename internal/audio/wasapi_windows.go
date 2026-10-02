// Package audio talks to Windows audio endpoints (the DAX virtual sound
// cards) through WASAPI shared mode. Every stream exchanges interleaved stereo
// float32 at 48 kHz with the rest of the program; any device-side format
// conversion happens here.
package audio

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"

	"github.com/va3mw/FlexTCI/internal/logx"
)

const Rate = 48000

type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// comThread pins the goroutine to an OS thread and initialises COM on it.
func comThread() (func(), error) {
	runtime.LockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 { // S_FALSE = already initialised
			runtime.UnlockOSThread()
			return nil, err
		}
	}
	return func() { ole.CoUninitialize(); runtime.UnlockOSThread() }, nil
}

func enumerator() (*wca.IMMDeviceEnumerator, error) {
	var e *wca.IMMDeviceEnumerator
	err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &e)
	return e, err
}

type rawDevice struct {
	Device
	mmd *wca.IMMDevice
}

// enum lists active endpoints. Caller must Release the returned mmd values.
func enum(flow uint32) ([]rawDevice, error) {
	e, err := enumerator()
	if err != nil {
		return nil, fmt.Errorf("device enumerator: %w", err)
	}
	defer e.Release()
	var dc *wca.IMMDeviceCollection
	if err := e.EnumAudioEndpoints(flow, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return nil, err
	}
	defer dc.Release()
	var n uint32
	dc.GetCount(&n)
	var out []rawDevice
	for i := uint32(0); i < n; i++ {
		var mmd *wca.IMMDevice
		if dc.Item(i, &mmd) != nil {
			continue
		}
		var id string
		mmd.GetId(&id)
		name := id
		var ps *wca.IPropertyStore
		if mmd.OpenPropertyStore(wca.STGM_READ, &ps) == nil {
			var pv wca.PROPVARIANT
			if ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv) == nil {
				name = pv.String()
			}
			ps.Release()
		}
		out = append(out, rawDevice{Device{ID: id, Name: name}, mmd})
	}
	return out, nil
}

// ListDevices returns capture (DAX RX) or render (DAX TX) endpoints.
func ListDevices(capture bool) ([]Device, error) {
	var out []Device
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, e := comThread()
		if e != nil {
			err = e
			return
		}
		defer release()
		flow := uint32(wca.ERender)
		if capture {
			flow = wca.ECapture
		}
		devs, e := enum(flow)
		if e != nil {
			err = e
			return
		}
		for _, d := range devs {
			out = append(out, d.Device)
			d.mmd.Release()
		}
	}()
	<-done
	return out, err
}

// AutoRXName returns the regexp used to find "DAX Audio RX n" automatically.
func AutoRXPattern(dax int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?i)\bDAX\b.*\bRX\s*%d\b`, dax))
}

// AutoTX finds the "DAX Audio TX" render device when none is chosen.
var AutoTX = regexp.MustCompile(`(?i)\bDAX\b.*\bTX\b`)

// Resolve picks a device by exact name, or by pattern when name is "".
func Resolve(devs []Device, name string, auto *regexp.Regexp) (Device, bool) {
	for _, d := range devs {
		if name != "" && d.Name == name {
			return d, true
		}
	}
	if name == "" && auto != nil {
		for _, d := range devs {
			if auto.MatchString(d.Name) && !strings.Contains(strings.ToUpper(d.Name), "IQ") {
				return d, true
			}
		}
	}
	return Device{}, false
}

func anyDAX(devs []Device) bool {
	for _, d := range devs {
		if strings.Contains(strings.ToUpper(d.Name), "DAX") {
			return true
		}
	}
	return false
}

// ---- streams ----

// Stream is a running capture or render loop. It reopens the device after
// errors until Stop is called.
type Stream struct {
	log  *logx.Source
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func (s *Stream) Stop() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// StartCapture streams audio from the named capture device to onData
// (stereo float32, 48 kHz). auto is used when name is "".
func StartCapture(l *logx.Source, name string, auto *regexp.Regexp, onData func([]float32)) *Stream {
	s := &Stream{log: l, stop: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.retry(func() error { return s.runCapture(name, auto, onData) })
	}()
	return s
}

// StartRender plays audio pulled from pull (stereo float32, 48 kHz) to the
// named render device.
func StartRender(l *logx.Source, name string, auto *regexp.Regexp, pull func([]float32)) *Stream {
	s := &Stream{log: l, stop: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.retry(func() error { return s.runRender(name, auto, pull) })
	}()
	return s
}

func (s *Stream) retry(run func() error) {
	for {
		err := run()
		select {
		case <-s.stop:
			return
		default:
		}
		s.log.Error("audio: %v; retrying in 3 s", err)
		select {
		case <-s.stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

type devFormat struct {
	ch     int
	rate   int
	bits   int
	float  bool
	native bool // true when Windows converts to our float/48k/stereo for us
}

// open activates and initialises an audio client in event-driven shared mode.
func (s *Stream) open(flow uint32, name string, auto *regexp.Regexp) (*wca.IAudioClient, devFormat, windows.Handle, string, error) {
	var f devFormat
	devs, err := enum(flow)
	if err != nil {
		return nil, f, 0, "", err
	}
	plain := make([]Device, len(devs))
	for i, d := range devs {
		plain[i] = d.Device
	}
	pick, ok := Resolve(plain, name, auto)
	var mmd *wca.IMMDevice
	for _, d := range devs {
		if ok && d.ID == pick.ID {
			mmd = d.mmd
		} else {
			d.mmd.Release()
		}
	}
	if mmd == nil {
		if name == "" && auto != nil {
			if !anyDAX(plain) {
				return nil, f, 0, "", fmt.Errorf("no DAX audio devices in Windows: start SmartSDR DAX (the DAX devices only exist while it runs)")
			}
			return nil, f, 0, "", fmt.Errorf("no DAX device matches %s; pick the device for this channel in FlexTCI", auto.String())
		}
		return nil, f, 0, "", fmt.Errorf("audio device %q not found (unplugged, disabled, or DAX not running?)", name)
	}
	defer mmd.Release()

	var ac *wca.IAudioClient
	if err := activate(mmd, wca.IID_IAudioClient, unsafe.Pointer(&ac)); err != nil {
		return nil, f, 0, "", fmt.Errorf("activate %s: %w", pick.Name, err)
	}

	const bufDur = 400000 // 40 ms in 100 ns units
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK | wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	want := wca.WAVEFORMATEX{WFormatTag: 3, NChannels: 2, NSamplesPerSec: Rate, WBitsPerSample: 32, NBlockAlign: 8, NAvgBytesPerSec: Rate * 8}
	err = ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, bufDur, 0, &want, nil)
	if err == nil {
		f = devFormat{ch: 2, rate: Rate, bits: 32, float: true, native: true}
	} else {
		// Fall back to the device mix format and convert ourselves.
		s.log.Debug("auto-convert init failed on %s (%v); using mix format", pick.Name, err)
		ac.Release()
		ac = nil
		if err := activate(mmd, wca.IID_IAudioClient, unsafe.Pointer(&ac)); err != nil {
			return nil, f, 0, "", err
		}
		var mix *wca.WAVEFORMATEX
		if err := ac.GetMixFormat(&mix); err != nil {
			ac.Release()
			return nil, f, 0, "", err
		}
		f = devFormat{ch: int(mix.NChannels), rate: int(mix.NSamplesPerSec), bits: int(mix.WBitsPerSample)}
		f.float = mix.WFormatTag == 3 || (mix.WFormatTag == 0xFFFE && f.bits == 32)
		err = ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK, bufDur, 0, mix, nil)
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
		if err != nil {
			ac.Release()
			return nil, f, 0, "", fmt.Errorf("initialize %s: %w", pick.Name, err)
		}
	}
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		ac.Release()
		return nil, f, 0, "", err
	}
	if err := ac.SetEventHandle(uintptr(ev)); err != nil {
		windows.CloseHandle(ev)
		ac.Release()
		return nil, f, 0, "", err
	}
	return ac, f, ev, pick.Name, nil
}

func activate(mmd *wca.IMMDevice, iid *ole.GUID, out unsafe.Pointer) error {
	hr, _, _ := syscall.SyscallN(mmd.VTable().Activate,
		uintptr(unsafe.Pointer(mmd)), uintptr(unsafe.Pointer(iid)), uintptr(wca.CLSCTX_ALL), 0, uintptr(out))
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

func (s *Stream) runCapture(name string, auto *regexp.Regexp, onData func([]float32)) error {
	release, err := comThread()
	if err != nil {
		return err
	}
	defer release()
	ac, f, ev, devName, err := s.open(wca.ECapture, name, auto)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ev)
	defer ac.Release()
	var cc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &cc); err != nil {
		return err
	}
	defer cc.Release()
	if err := ac.Start(); err != nil {
		return err
	}
	defer ac.Stop()
	s.log.Info("capture open: %s (%d ch, %d Hz, %d-bit%s)", devName, f.ch, f.rate, f.bits, convNote(f))
	rs := NewResampler(f.rate, Rate, 2)
	for {
		select {
		case <-s.stop:
			return nil
		default:
		}
		windows.WaitForSingleObject(ev, 200)
		for {
			var n uint32
			if err := cc.GetNextPacketSize(&n); err != nil {
				return fmt.Errorf("capture %s: %w", devName, err)
			}
			if n == 0 {
				break
			}
			var data *byte
			var frames, flags uint32
			var pos, qpc uint64
			if err := cc.GetBuffer(&data, &frames, &flags, &pos, &qpc); err != nil {
				return fmt.Errorf("capture %s: %w", devName, err)
			}
			out := toStereoFloat(data, int(frames), f, flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0)
			cc.ReleaseBuffer(frames)
			if f.rate != Rate {
				out = rs.Process(out)
			}
			onData(out)
		}
	}
}

func (s *Stream) runRender(name string, auto *regexp.Regexp, pull func([]float32)) error {
	release, err := comThread()
	if err != nil {
		return err
	}
	defer release()
	ac, f, ev, devName, err := s.open(wca.ERender, name, auto)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ev)
	defer ac.Release()
	var rc *wca.IAudioRenderClient
	if err := ac.GetService(wca.IID_IAudioRenderClient, &rc); err != nil {
		return err
	}
	defer rc.Release()
	var bufFrames uint32
	ac.GetBufferSize(&bufFrames)
	// Pre-fill with silence so the device starts cleanly.
	var data *byte
	if rc.GetBuffer(bufFrames, &data) == nil {
		rc.ReleaseBuffer(bufFrames, 2) // AUDCLNT_BUFFERFLAGS_SILENT
	}
	if err := ac.Start(); err != nil {
		return err
	}
	defer ac.Stop()
	s.log.Info("render open: %s (%d ch, %d Hz, %d-bit%s, buffer %d frames)", devName, f.ch, f.rate, f.bits, convNote(f), bufFrames)
	rs := NewResampler(Rate, f.rate, 2)
	var pending []float32 // resampled audio not yet written (fallback path)
	for {
		select {
		case <-s.stop:
			return nil
		default:
		}
		windows.WaitForSingleObject(ev, 200)
		var pad uint32
		if err := ac.GetCurrentPadding(&pad); err != nil {
			return fmt.Errorf("render %s: %w", devName, err)
		}
		avail := int(bufFrames - pad)
		if avail <= 0 {
			continue
		}
		var src []float32
		if f.rate == Rate {
			src = make([]float32, avail*2)
			pull(src)
		} else {
			for len(pending) < avail*2 {
				chunk := make([]float32, 480*2)
				pull(chunk)
				pending = append(pending, rs.Process(chunk)...)
			}
			src = pending[:avail*2]
			pending = append([]float32(nil), pending[avail*2:]...)
		}
		if err := rc.GetBuffer(uint32(avail), &data); err != nil {
			return fmt.Errorf("render %s: %w", devName, err)
		}
		fromStereoFloat(data, src, avail, f)
		rc.ReleaseBuffer(uint32(avail), 0)
	}
}

func convNote(f devFormat) string {
	if f.native {
		return ", windows converts"
	}
	return ", converted here"
}

// toStereoFloat converts a device buffer into interleaved stereo float32.
func toStereoFloat(data *byte, frames int, f devFormat, silent bool) []float32 {
	out := make([]float32, frames*2)
	if silent || frames == 0 || data == nil {
		return out
	}
	n := frames * f.ch
	sample := func(i int) float32 { return 0 }
	switch {
	case f.float && f.bits == 32:
		b := unsafe.Slice((*float32)(unsafe.Pointer(data)), n)
		sample = func(i int) float32 { return b[i] }
	case f.bits == 16:
		b := unsafe.Slice((*int16)(unsafe.Pointer(data)), n)
		sample = func(i int) float32 { return float32(b[i]) / 32768 }
	case f.bits == 32:
		b := unsafe.Slice((*int32)(unsafe.Pointer(data)), n)
		sample = func(i int) float32 { return float32(b[i]) / 2147483648 }
	case f.bits == 24:
		b := unsafe.Slice(data, n*3)
		sample = func(i int) float32 {
			v := int32(b[i*3]) | int32(b[i*3+1])<<8 | int32(int8(b[i*3+2]))<<16
			return float32(v) / 8388608
		}
	}
	for i := 0; i < frames; i++ {
		l := sample(i * f.ch)
		r := l
		if f.ch > 1 {
			r = sample(i*f.ch + 1)
		}
		out[i*2], out[i*2+1] = l, r
	}
	return out
}

// fromStereoFloat writes interleaved stereo float32 into a device buffer.
func fromStereoFloat(data *byte, src []float32, frames int, f devFormat) {
	n := frames * f.ch
	for i := 0; i < frames; i++ {
		l, r := src[i*2], src[i*2+1]
		for c := 0; c < f.ch; c++ {
			v := l
			if c == 1 {
				v = r
			} else if c > 1 {
				v = 0
			}
			j := i*f.ch + c
			switch {
			case f.float && f.bits == 32:
				unsafe.Slice((*float32)(unsafe.Pointer(data)), n)[j] = v
			case f.bits == 16:
				unsafe.Slice((*int16)(unsafe.Pointer(data)), n)[j] = int16(clamp(v) * 32767)
			case f.bits == 32:
				unsafe.Slice((*int32)(unsafe.Pointer(data)), n)[j] = int32(clamp(v) * 2147483647)
			case f.bits == 24:
				b := unsafe.Slice(data, n*3)
				x := int32(clamp(v) * 8388607)
				b[j*3], b[j*3+1], b[j*3+2] = byte(x), byte(x>>8), byte(x>>16)
			}
		}
	}
}

func clamp(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}
