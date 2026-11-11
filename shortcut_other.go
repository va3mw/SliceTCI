//go:build !windows

package main

import "errors"

func createDesktopShortcut(string) (string, error) {
	return "", errors.New("desktop shortcuts are only supported on Windows")
}

const elevatedInstallFlag = "-elevated-install"

func installElevated(string, string) error {
	return errors.New("permission denied")
}

func runElevatedInstall([]string) int { return 2 }
