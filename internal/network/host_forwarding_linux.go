//go:build linux

package network

import (
	"context"
	"fmt"
	"hash/fnv"
	"os/exec"
	"strconv"
	"strings"

	"github.com/zanel1u/cloud-cli-proxy/internal/agentapi"
)

const portMapChain = "CLOUDPROXY-PORTMAP"

func hostNetnsCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	nsenterArgs := append([]string{"-t", "1", "-m", "-n", "--", name}, args...)
	return exec.CommandContext(ctx, "nsenter", nsenterArgs...)
}

func runHostNetnsCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := hostNetnsCommand(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func ensureIPForwarding(ctx context.Context) error {
	if out, err := runHostNetnsCommand(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enable ip forwarding: %w (%s)", err, out)
	}
	return nil
}

func ensureHostMasquerade(ctx context.Context) error {
	return appendUniqueRule(ctx, "nat", "POSTROUTING", []string{
		"-s", "10.99.0.0/16", "-j", "MASQUERADE",
	})
}

// setupPortForwarding creates host iptables rules for worker routing, and starts
// Go userland TCP proxies for host port mapping.
//
// Architecture:
//   - Worker's default gateway points to the host's bridge IP (10.99.X.1).
//   - All worker outbound traffic reaches the host first.
//   - Host policy-routes worker subnet traffic to the gateway (10.99.X.2) -> sing-box tunnel.
//   - Port-mapped inbound traffic is handled by Go TCP proxies:
//     each proxy listens in the host netns and forwards to the worker isolated IP,
//     avoiding kernel-specific nat:PREROUTING behavior for locally-destined traffic.
func setupPortForwarding(ctx context.Context, hostID, bridgeGW, gwIP string, ports []agentapi.PortMapping) error {
	third := subnetThirdOctet(hostID)
	workerIP := fmt.Sprintf("10.99.%d.3", third)
	subnet := fmt.Sprintf("10.99.%d.0/24", third)
	hostChain := hostPortMapChain(hostID)

	if err := ensureHostPortMapChains(ctx, hostChain); err != nil {
		return err
	}
	if err := flushHostPortMapChains(ctx, hostChain); err != nil {
		return err
	}

	// Remove rules left by older versions where per-host rules lived directly
	// in CLOUDPROXY-PORTMAP. New rules are isolated in the per-host chain.
	deleteRulesByNeedle(ctx, "nat", portMapChain, workerIP)
	deleteRulesByNeedle(ctx, "filter", portMapChain, workerIP)
	deleteRulesByNeedle(ctx, "filter", portMapChain, subnet)

	// Port forwarding is handled by a Go userland proxy. iptables DNAT/SNAT
	// rules from older builds are removed above/below; only FORWARD allow rules
	// remain so control-plane can reach worker isolated IPs.
	deleteNatRulesByComment(ctx, "cloudproxy-snat-"+hostID)
	for _, pm := range ports {
		if pm.HostPort <= 0 || pm.ContainerPort <= 0 {
			continue
		}

		proto := strings.ToLower(pm.Protocol)
		if proto == "" {
			proto = "tcp"
		}

		cp := strconv.Itoa(pm.ContainerPort)

		// Allow control-plane/userland proxy traffic to worker isolated IP.
		fwdRule := []string{
			"-p", proto, "--dport", cp,
			"-d", workerIP,
			"-j", "ACCEPT",
		}
		if err := appendUniqueRule(ctx, "filter", hostChain, fwdRule); err != nil {
			return fmt.Errorf("iptables FORWARD %s:%d: %w", workerIP, pm.ContainerPort, err)
		}
	}

	// --- FORWARD chain: allow all worker subnet traffic ---
	// Worker traffic (DNS, HTTP, etc.) reaches the host first because the
	// default gateway is the host bridge IP. The host policy-routes it to
	// the gateway for sing-box tunneling.
	if err := appendUniqueRule(ctx, "filter", hostChain, []string{
		"-s", subnet,
		"-j", "ACCEPT",
	}); err != nil {
		return fmt.Errorf("iptables worker subnet forward: %w", err)
	}

	// --- Policy routing: worker subnet -> gateway -> sing-box tunnel ---
	tableID := strconv.Itoa(10000 + third)
	if out, err := runHostNetnsCommand(ctx, "ip", "rule", "add", "from", subnet, "lookup", tableID); err != nil && !strings.Contains(out, "File exists") {
		return fmt.Errorf("add policy rule from %s lookup %s: %w (%s)", subnet, tableID, err, out)
	}
	if out, err := runHostNetnsCommand(ctx, "ip", "route", "replace", "default", "via", gwIP, "table", tableID); err != nil {
		return fmt.Errorf("replace policy route table %s via %s: %w (%s)", tableID, gwIP, err, out)
	}

	if err := startPortForwarders(hostID, workerIP, ports); err != nil {
		return fmt.Errorf("start port forwarders: %w", err)
	}

	return nil
}

// ensurePortMapChain creates the CLOUDPROXY-PORTMAP iptables chain and hooks
// it into PREROUTING (nat) and FORWARD (filter) if not already present.
func ensurePortMapChain(ctx context.Context) error {
	// Create chain (ignore "already exists" error)
	hostNetnsCommand(ctx, "iptables", "-t", "nat", "-N", portMapChain).Run()
	hostNetnsCommand(ctx, "iptables", "-N", portMapChain).Run()

	// Hook into PREROUTING (nat) if not already present
	if err := ensureChainHook(ctx, "nat", "PREROUTING", portMapChain); err != nil {
		return err
	}
	// Hook into FORWARD (filter) if not already present
	if err := ensureChainHook(ctx, "filter", "FORWARD", portMapChain); err != nil {
		return err
	}

	return nil
}

func ensureHostPortMapChains(ctx context.Context, hostChain string) error {
	hostNetnsCommand(ctx, "iptables", "-t", "nat", "-N", hostChain).Run()
	hostNetnsCommand(ctx, "iptables", "-N", hostChain).Run()

	if err := appendUniqueRule(ctx, "nat", portMapChain, []string{"-j", hostChain}); err != nil {
		return fmt.Errorf("hook nat/%s->%s: %w", portMapChain, hostChain, err)
	}
	if err := appendUniqueRule(ctx, "filter", portMapChain, []string{"-j", hostChain}); err != nil {
		return fmt.Errorf("hook filter/%s->%s: %w", portMapChain, hostChain, err)
	}
	return nil
}

func flushHostPortMapChains(ctx context.Context, hostChain string) error {
	if out, err := runHostNetnsCommand(ctx, "iptables", "-t", "nat", "-F", hostChain); err != nil {
		return fmt.Errorf("flush nat/%s: %w (%s)", hostChain, err, out)
	}
	if out, err := runHostNetnsCommand(ctx, "iptables", "-F", hostChain); err != nil {
		return fmt.Errorf("flush filter/%s: %w (%s)", hostChain, err, out)
	}
	return nil
}

func ensureChainHook(ctx context.Context, table, parent, child string) error {
	checkArgs := iptablesArgs(table, "-C", parent, "-j", child)
	if hostNetnsCommand(ctx, "iptables", checkArgs...).Run() == nil {
		return nil
	}

	addArgs := iptablesArgs(table, "-I", parent, "1", "-j", child)
	if out, err := runHostNetnsCommand(ctx, "iptables", addArgs...); err != nil {
		return fmt.Errorf("hook %s/%s->%s: %w (%s)", table, parent, child, err, out)
	}
	return nil
}

// teardownPortForwarding removes this host's listeners, jump rules, per-host
// chains, legacy SNAT rules, and policy routes. The shared CLOUDPROXY-PORTMAP
// hook remains installed because other hosts may still be using it.
func teardownPortForwarding(ctx context.Context, hostID string) {
	stopPortForwarders(hostID)

	third := subnetThirdOctet(hostID)
	gwIP := fmt.Sprintf("10.99.%d.2", third)
	workerIP := fmt.Sprintf("10.99.%d.3", third)
	subnet := fmt.Sprintf("10.99.%d.0/24", third)
	tableID := strconv.Itoa(10000 + third)
	hostChain := hostPortMapChain(hostID)

	// Remove SNAT rules by matching the comment
	deleteNatRulesByComment(ctx, "cloudproxy-snat-"+hostID)

	// Remove per-host jumps and chains. Keep the shared CLOUDPROXY-PORTMAP
	// hook/chain because other hosts may still be using it.
	deleteJumpRules(ctx, "nat", portMapChain, hostChain)
	deleteJumpRules(ctx, "filter", portMapChain, hostChain)
	hostNetnsCommand(ctx, "iptables", "-t", "nat", "-F", hostChain).Run()
	hostNetnsCommand(ctx, "iptables", "-t", "nat", "-X", hostChain).Run()
	hostNetnsCommand(ctx, "iptables", "-F", hostChain).Run()
	hostNetnsCommand(ctx, "iptables", "-X", hostChain).Run()

	// Best-effort cleanup for rules created by older builds before per-host
	// chains existed.
	deleteRulesByNeedle(ctx, "nat", portMapChain, workerIP)
	deleteRulesByNeedle(ctx, "filter", portMapChain, workerIP)
	deleteRulesByNeedle(ctx, "filter", portMapChain, subnet)

	// Clean up policy routes
	hostNetnsCommand(ctx, "ip", "rule", "del", "from", subnet, "lookup", tableID).Run()
	hostNetnsCommand(ctx, "ip", "route", "del", "default", "via", gwIP, "table", tableID).Run()
}

// deleteNatRulesByComment removes all rules in the POSTROUTING chain whose
// comment contains the given substring. It iterates from the end to avoid
// line-number shifts.
func deleteNatRulesByComment(ctx context.Context, comment string) {
	out, _ := hostNetnsCommand(ctx, "iptables", "-t", "nat", "-L", "POSTROUTING", "--line-numbers", "-n").CombinedOutput()
	lines := strings.Split(string(out), "\n")
	// Iterate backwards so line numbers don't shift
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], comment) {
			fields := strings.Fields(lines[i])
			if len(fields) > 0 {
				hostNetnsCommand(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING", fields[0]).Run()
			}
		}
	}
}

func appendUniqueRule(ctx context.Context, table, chain string, rule []string) error {
	checkArgs := append(iptablesArgs(table, "-C", chain), rule...)
	if hostNetnsCommand(ctx, "iptables", checkArgs...).Run() == nil {
		return nil
	}
	addArgs := append(iptablesArgs(table, "-A", chain), rule...)
	if out, err := runHostNetnsCommand(ctx, "iptables", addArgs...); err != nil {
		return fmt.Errorf("%s/%s append %s: %w (%s)", table, chain, strings.Join(rule, " "), err, out)
	}
	return nil
}

func deleteJumpRules(ctx context.Context, table, parent, child string) {
	for {
		args := iptablesArgs(table, "-D", parent, "-j", child)
		if hostNetnsCommand(ctx, "iptables", args...).Run() != nil {
			return
		}
	}
}

func deleteRulesByNeedle(ctx context.Context, table, chain, needle string) {
	if needle == "" {
		return
	}
	out, _ := runHostNetnsCommand(ctx, "iptables", iptablesArgs(table, "-S", chain)...)
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != chain {
			continue
		}
		args := iptablesArgs(table, append([]string{"-D", chain}, fields[2:]...)...)
		hostNetnsCommand(ctx, "iptables", args...).Run()
	}
}

func iptablesArgs(table string, args ...string) []string {
	if table == "" {
		return args
	}
	return append([]string{"-t", table}, args...)
}

func hostPortMapChain(hostID string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(hostID))
	return fmt.Sprintf("CPH-%016x", h.Sum64())
}
