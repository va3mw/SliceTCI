//go:build !windows

package main

import "errors"

func createDesktopShortcut(string) (string, error) {
	return "", errors.New("desktop shortcuts are only supported on Windows")
}
