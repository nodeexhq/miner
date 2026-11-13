package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServicePlist(t *testing.T) {
	// Paths with spaces and XML metacharacters must survive intact.
	exe := "/Applications/Node Ex & Co/nodeex-miner"
	dir := "/Users/someone/Library/Application Support/NodeEx/<miner>"
	plist := servicePlist(exe, dir, []string{"-ssh-target", "192.168.64.5:22"})

	text := string(plist)
	for _, want := range []string{
		"<string>/Applications/Node Ex &amp; Co/nodeex-miner</string>",
		"<string>run</string>",
		"NodeEx/&lt;miner&gt;</string>",
		"<string>192.168.64.5:22</string>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist lacks %q", want)
		}
	}

	if runtime.GOOS != "darwin" {
		return
	}
	// Have macOS itself parse it and read the arguments back.
	path := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(path, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil rejects the plist: %s", out)
	}
	out, err := exec.Command("plutil", "-extract", "ProgramArguments", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := `["\/Applications\/Node Ex & Co\/nodeex-miner","run","-dir","\/Users\/someone\/Library\/Application Support\/NodeEx\/<miner>","-ssh-target","192.168.64.5:22"]`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("arguments read back as\n%s\nwant\n%s", got, want)
	}
}
