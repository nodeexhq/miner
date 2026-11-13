package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
)

// The Miner runs as a per-user launchd agent: it starts when its user logs
// in and is restarted if it exits. A node that must come back unattended
// after a power cut therefore needs automatic login enabled for that user.
const serviceLabel = "com.nodeex.miner"

func servicePlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}

// servicePlist renders the launchd job that runs `exe run -dir dir`.
func servicePlist(exe, dir string, extraArgs []string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var args bytes.Buffer
	for _, a := range append([]string{exe, "run", "-dir", dir}, extraArgs...) {
		fmt.Fprintf(&args, "\t\t<string>%s</string>\n", esc(a))
	}
	logPath := esc(filepath.Join(dir, "miner.log"))
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + serviceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + args.String() + `	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + logPath + `</string>
	<key>StandardErrorPath</key>
	<string>` + logPath + `</string>
</dict>
</plist>
`)
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

func cmdService(args []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("the service commands are only supported on macOS")
	}
	if len(args) == 0 {
		return errors.New("usage: nodeex-miner service <install|uninstall|status> [flags] [-- run flags]")
	}
	fs := flag.NewFlagSet("service "+args[0], flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args[1:])

	plist, err := servicePlistPath()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())

	switch args[0] {
	case "install":
		if _, err := loadIdentity(*dir); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		absDir, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(plist, servicePlist(exe, absDir, fs.Args()), 0o644); err != nil {
			return err
		}
		// Replace any copy already loaded, then start it.
		launchctl("bootout", domain+"/"+serviceLabel)
		if err := launchctl("bootstrap", domain, plist); err != nil {
			return err
		}
		fmt.Printf("installed: the Miner now starts at login and restarts if it stops\n  job:  %s\n  log:  %s\n", plist, filepath.Join(absDir, "miner.log"))
		fmt.Println("for the node to return unattended after a restart, enable automatic login for this user")
		return nil
	case "uninstall":
		launchctl("bootout", domain+"/"+serviceLabel)
		if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Println("uninstalled: the Miner no longer starts automatically")
		return nil
	case "status":
		if _, err := os.Stat(plist); errors.Is(err, os.ErrNotExist) {
			fmt.Println("not installed")
			return nil
		}
		if err := launchctl("print", domain+"/"+serviceLabel); err != nil {
			fmt.Println("installed but not loaded")
			return nil
		}
		fmt.Println("installed and loaded")
		return nil
	}
	return fmt.Errorf("unknown service command %q", args[0])
}
