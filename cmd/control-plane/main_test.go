package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultSSHProxyHostKeyPathUsesDataDir(t *testing.T) {
	t.Setenv("DATA_DIR", "/tmp/cloudproxy-data")

	got := defaultSSHProxyHostKeyPath()
	want := filepath.Join("/tmp/cloudproxy-data", "ssh-proxy", "ssh_host_ed25519_key")
	if got != want {
		t.Fatalf("defaultSSHProxyHostKeyPath() = %q, want %q", got, want)
	}
}

func TestDefaultSSHProxyHostKeyPathFallback(t *testing.T) {
	t.Setenv("DATA_DIR", "")

	got := defaultSSHProxyHostKeyPath()
	want := filepath.Join("/var/lib/cloud-cli-proxy", "ssh-proxy", "ssh_host_ed25519_key")
	if got != want {
		t.Fatalf("defaultSSHProxyHostKeyPath() = %q, want %q", got, want)
	}
}
