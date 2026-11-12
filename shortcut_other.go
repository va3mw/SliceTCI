//go:build !windows

package main

import "errors"

func createDesktopShortcut(string) (string, error) {
	return "", errors.New("desktop shortcuts are only supported on Windows")
}

func makeDesktopShortcut(string, string, string, string) (string, error) {
	return "", errors.New("desktop shortcuts are only supported on Windows")
}

const elevatedInstallFlag = "-elevated-install"

func installElevated(string, string) error {
	return errors.New("permission denied")
}

func runElevatedInstall([]string) int { return 2 }

func wsjtxFromRegistry() []string { return nil }
