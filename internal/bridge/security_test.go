package bridge

import (
	"net/http"
	"testing"

	"github.com/va3mw/FlexTCI/internal/tci"
)

func TestAllowedOrigin(t *testing.T) {
	cases := map[string]bool{
		"":                          true, // native clients (WSJT-X, Hamlib)
		"http://localhost:8080":     true,
		"http://127.0.0.1":          true,
		"http://[::1]:3000":         true,
		"https://evil.example":      false, // drive-by web page
		"http://127.0.0.1.evil.com": false,
		"null":                      false, // file:// and sandboxed pages
	}
	for origin, want := range cases {
		r, _ := http.NewRequest("GET", "/", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if got := allowedOrigin(r); got != want {
			t.Errorf("origin %q: got %v, want %v", origin, got, want)
		}
	}
}

// A client-supplied sample rate must not be able to blow up memory.
func TestTXRateClamped(t *testing.T) {
	c := &client{rate: 48000, chans: 2}
	in := make([]float32, 2048)
	for _, bad := range []uint32{1, 7, 1 << 31} {
		out := c.toStereo48k(tci.Header{SampleRate: bad, Channels: 2}, in)
		if len(out) > len(in) {
			t.Errorf("rate %d: %d samples out for %d in", bad, len(out), len(in))
		}
	}
}
