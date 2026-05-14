package network

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// BuildGatewaySingBoxConfig returns the effective sing-box JSON used by a
// gateway container. gatewayConfigRaw is a host-level override:
//   - empty/null: use the bound egress IP outbound config
//   - sing-box outbound JSON: wrap it in the managed tun gateway template
//   - full sing-box config JSON: validate and use the user config as-is
func BuildGatewaySingBoxConfig(outboundRaw, gatewayConfigRaw json.RawMessage, dnsServer, proxyServerIP string) ([]byte, error) {
	gatewayConfigRaw = bytes.TrimSpace(gatewayConfigRaw)
	if len(gatewayConfigRaw) == 0 || bytes.Equal(gatewayConfigRaw, []byte("null")) {
		return buildGatewaySingBoxConfig(outboundRaw, dnsServer, proxyServerIP)
	}

	full, err := gatewayConfigLooksFull(gatewayConfigRaw)
	if err != nil {
		return nil, err
	}
	if full {
		return BuildGatewayCustomSingBoxConfig(gatewayConfigRaw)
	}

	if overrideDNS := gatewayOutboundDNSServer(gatewayConfigRaw); overrideDNS != "" {
		dnsServer = overrideDNS
	}
	return buildGatewaySingBoxConfig(gatewayConfigRaw, dnsServer, proxyServerIP)
}

// BuildGatewayCustomSingBoxConfig validates a full host-supplied sing-box
// gateway config and returns it without injecting generated routes or outbounds.
func BuildGatewayCustomSingBoxConfig(raw json.RawMessage) ([]byte, error) {
	return buildGatewayFullSingBoxConfig(raw)
}

func ResolveGatewayProxyServerIP(outboundRaw, gatewayConfigRaw json.RawMessage) (string, error) {
	serverSource := outboundRaw
	if !gatewayConfigEmpty(gatewayConfigRaw) {
		full, err := gatewayConfigLooksFull(gatewayConfigRaw)
		if err != nil {
			return "", err
		}
		if full {
			return "", nil
		}
		serverSource = gatewayConfigRaw
	}

	if gatewayConfigEmpty(serverSource) {
		return "", nil
	}
	serverIP, _, err := extractProxyServer(serverSource)
	if err != nil {
		return "", err
	}
	return serverIP, nil
}

func gatewayConfigLooksFull(raw json.RawMessage) (bool, error) {
	if gatewayConfigEmpty(raw) {
		return false, nil
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, fmt.Errorf("parse gateway_config: %w", err)
	}

	_, hasInbounds := parsed["inbounds"]
	_, hasOutbounds := parsed["outbounds"]
	_, hasRoute := parsed["route"]
	return hasInbounds || hasOutbounds || hasRoute, nil
}

func gatewayConfigEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func gatewayOutboundDNSServer(raw json.RawMessage) string {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	dnsServer, _ := parsed["dns_server"].(string)
	return dnsServer
}

func buildGatewayFullSingBoxConfig(raw json.RawMessage) ([]byte, error) {
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse full gateway_config: %w", err)
	}

	inbounds, ok := cfg["inbounds"].([]any)
	if !ok || len(inbounds) == 0 {
		return nil, fmt.Errorf("full gateway_config must include inbounds")
	}
	if !hasTunInbound(inbounds) {
		return nil, fmt.Errorf("full gateway_config must include a tun inbound")
	}

	outbounds, ok := cfg["outbounds"].([]any)
	if !ok || len(outbounds) == 0 {
		return nil, fmt.Errorf("full gateway_config must include outbounds")
	}

	route, ok := cfg["route"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("full gateway_config must include route")
	}
	if final, _ := route["final"].(string); final == "" {
		return nil, fmt.Errorf("full gateway_config route.final is required")
	}

	return json.MarshalIndent(cfg, "", "  ")
}

func hasTunInbound(inbounds []any) bool {
	for _, item := range inbounds {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["type"].(string); typ == "tun" {
			return true
		}
	}
	return false
}

// buildGatewaySingBoxConfig builds sing-box JSON for the sidecar gateway (tun mode).
// tun + auto_route captures all forwarded traffic from the worker container.
func buildGatewaySingBoxConfig(outboundRaw json.RawMessage, dnsServer, proxyServerIP string) ([]byte, error) {
	if dnsServer == "" {
		dnsServer = "1.1.1.1"
	}

	proxyOut, err := buildGatewayProxyOutbound(outboundRaw, proxyServerIP)
	if err != nil {
		return nil, err
	}
	directOut, err := json.Marshal(map[string]any{
		"type": "direct",
		"tag":  "direct",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal direct outbound: %w", err)
	}

	tunIn, err := json.Marshal(map[string]any{
		"type":                       "tun",
		"tag":                        "tun-in",
		"address":                    []string{"172.19.0.1/30"},
		"auto_route":                 true,
		"strict_route":               false,
		"stack":                      "mixed",
		"sniff":                      true,
		"sniff_override_destination": true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tun inbound: %w", err)
	}

	routeRules := make([]map[string]any, 0, 2)
	if proxyServerIP != "" {
		routeRules = append(routeRules, map[string]any{"ip_cidr": []string{proxyServerIP + "/32"}, "outbound": "direct"})
	}
	routeRules = append(routeRules, map[string]any{"port": 53, "action": "hijack-dns"})

	cfg := map[string]any{
		"log": map[string]any{"level": "info"},
		"dns": map[string]any{
			"servers": []map[string]any{
				{"tag": "dns-remote", "type": "tcp", "server": dnsServer, "detour": "proxy-out"},
			},
			"strategy": "ipv4_only",
		},
		"inbounds":  []json.RawMessage{json.RawMessage(tunIn)},
		"outbounds": []json.RawMessage{proxyOut, directOut},
		"route": map[string]any{
			"rules":                 routeRules,
			"final":                 "proxy-out",
			"auto_detect_interface": true,
		},
	}

	return json.MarshalIndent(cfg, "", "  ")
}

func buildGatewayProxyOutbound(userConfig json.RawMessage, resolvedIP string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(userConfig, &m); err != nil {
		return nil, fmt.Errorf("parse outbound config: %w", err)
	}
	delete(m, "dns_server")
	delete(m, "bind_interface")
	m["tag"] = "proxy-out"
	if resolvedIP != "" {
		m["server"] = resolvedIP
	}

	if tls, ok := m["tls"].(map[string]any); ok {
		if reality, ok := tls["reality"].(map[string]any); ok {
			if enabled, _ := reality["enabled"].(bool); enabled {
				if _, hasUtls := tls["utls"]; !hasUtls {
					tls["utls"] = map[string]any{"enabled": true, "fingerprint": "chrome"}
				}
			}
		}
	}

	return json.Marshal(m)
}
