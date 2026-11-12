package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// wsjtxFromRegistry finds wsjtx.exe through the installer's uninstall
// entries (per machine, 32-bit view and per user).
func wsjtxFromRegistry() []string {
	var out []string
	const uninst = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	for _, root := range []struct {
		k    registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, uninst},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, uninst},
	} {
		k, err := registry.OpenKey(root.k, root.path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			sk, err := registry.OpenKey(root.k, root.path+`\`+n, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, _ := sk.GetStringValue("DisplayName")
			if strings.Contains(strings.ToUpper(name), "WSJT-X") {
				for _, v := range []string{"InstallLocation", "UninstallString", "DisplayIcon"} {
					s, _, err := sk.GetStringValue(v)
					if err != nil || s == "" {
						continue
					}
					s = strings.Trim(strings.SplitN(s, ",", 2)[0], `" `)
					dir := s
					if strings.HasSuffix(strings.ToLower(s), ".exe") {
						dir = filepath.Dir(s)
					}
					if strings.EqualFold(filepath.Base(dir), "bin") {
						dir = filepath.Dir(dir)
					}
					out = append(out, filepath.Join(dir, "bin", wsjtxExe))
				}
			}
			sk.Close()
		}
	}
	return out
}
