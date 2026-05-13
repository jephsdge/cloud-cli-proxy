//go:build linux

package network

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestHostNetnsCommandUsesHostNetworkNamespace(t *testing.T) {
	cmd := hostNetnsCommand(context.Background(), "ip", "rule", "show")
	want := []string{"nsenter", "-t", "1", "-m", "-n", "--", "ip", "rule", "show"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("unexpected command args: got %#v want %#v", cmd.Args, want)
	}
}

func TestHostPortMapChainIsStableAndShort(t *testing.T) {
	first := hostPortMapChain("host-1")
	second := hostPortMapChain("host-1")
	if first != second {
		t.Fatalf("expected stable chain name, got %q and %q", first, second)
	}
	if !strings.HasPrefix(first, "CPH-") {
		t.Fatalf("expected CPH- prefix, got %q", first)
	}
	if len(first) > 28 {
		t.Fatalf("chain name too long for iptables: %q", first)
	}
}
