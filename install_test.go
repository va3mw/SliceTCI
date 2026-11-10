package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallTo(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Downloads", "SliceTCI-0.5.3.exe")
	os.MkdirAll(filepath.Dir(src), 0o755)
	os.WriteFile(src, []byte("new"), 0o644)
	dst := filepath.Join(dir, "AppData", exeName)

	if err := installTo(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Fatalf("installed exe holds %q", b)
	}

	// Installing again over an existing (possibly running) exe moves it aside.
	os.WriteFile(src, []byte("newer"), 0o644)
	if err := installTo(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "newer" {
		t.Fatalf("installed exe holds %q", b)
	}
	if b, _ := os.ReadFile(dst + ".old"); string(b) != "new" {
		t.Fatalf("previous exe not kept as .old: %q", b)
	}
	cleanupOldExe(dst)
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Fatal(".old not removed")
	}

	if err := installTo(dst, dst); err == nil {
		t.Fatal("installing onto itself should fail")
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.5.4", "0.5.3", true},
		{"0.10.0", "0.9.9", true},
		{"0.5.3", "0.5.3", false},
		{"0.5.3", "0.5.10", false},
		{"1.0", "0.9.9", true},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// TestFetchAndDownload: the release's exe asset is found by name, and the
// download is accepted only if it matches the published SHA-256.
func TestFetchAndDownload(t *testing.T) {
	exe := []byte(strings.Repeat("x", 2<<20))
	sum := sha256.Sum256(exe)
	digest := hex.EncodeToString(sum[:])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.6.0","assets":[
				{"name":"notes.txt","digest":"sha256:%s","browser_download_url":"%s/notes"},
				{"name":"SliceTCI-0.6.0.exe","digest":"sha256:%s","browser_download_url":"%s/exe"}]}`,
				digest, srv.URL, digest, srv.URL)
		case "/exe":
			w.Write(exe)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	info, err := fetchRelease(srv.URL + "/latest")
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.6.0" || info.SHA256 != digest || info.URL != srv.URL+"/exe" {
		t.Fatalf("bad release info %+v", info)
	}
	if _, err := fetchRelease(srv.URL + "/missing"); err == nil || !strings.Contains(err.Error(), "no public release") {
		t.Fatalf("404 should say there is no public release, got %v", err)
	}

	target := filepath.Join(t.TempDir(), exeName)
	p, err := downloadUpdate(info, target)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); len(b) != len(exe) {
		t.Fatalf("downloaded %d bytes", len(b))
	}

	info.SHA256 = strings.Repeat("0", 64)
	if _, err := downloadUpdate(info, target); err == nil {
		t.Fatal("a download with the wrong checksum was accepted")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Fatal("rejected download left behind")
	}
}
