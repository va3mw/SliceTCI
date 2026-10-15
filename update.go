package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// releasesAPI is the latest GitHub release. It only answers while the
// repository is public; a private repository returns 404.
const releasesAPI = "https://api.github.com/repos/va3mw/FlexTCI/releases/latest"

type updateInfo struct {
	Version string
	SHA256  string
	URL     string // download URL of the exe
}

// fetchRelease reads the latest release: its tag (v0.5.3) is the version, and
// its exe asset (FlexTCI-0.5.3.exe) carries the SHA-256 GitHub recorded.
func fetchRelease(apiURL string) (updateInfo, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return updateInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "FlexTCI/"+version)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return updateInfo{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return updateInfo{}, errors.New("no public release found on GitHub")
	default:
		return updateInfo{}, fmt.Errorf("GitHub releases: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
			URL    string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return updateInfo{}, err
	}
	for _, a := range rel.Assets {
		sum, ok := strings.CutPrefix(a.Digest, "sha256:")
		if strings.HasPrefix(a.Name, "FlexTCI") && strings.HasSuffix(a.Name, ".exe") && ok && len(sum) == 64 && a.URL != "" {
			return updateInfo{Version: strings.TrimPrefix(rel.TagName, "v"), SHA256: strings.ToLower(sum), URL: a.URL}, nil
		}
	}
	return updateInfo{}, errors.New("the latest release has no FlexTCI exe with a checksum")
}

// newerVersion reports whether version a is newer than b ("0.10.0" > "0.9.2").
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// downloadUpdate saves the new exe next to exePath as <exe>.new and checks
// its SHA-256 against the published one.
func downloadUpdate(info updateInfo, exePath string) (string, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(info.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: %s", resp.Status)
	}
	tmp := exePath + ".new"
	f, err := os.Create(tmp)
	if err != nil {
		return "", fmt.Errorf("cannot write next to the exe: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 100<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != info.SHA256 {
		err = errors.New("the download does not match the published checksum")
	}
	if err == nil && n < 1<<20 {
		err = errors.New("the download is too small to be FlexTCI")
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}
