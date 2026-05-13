package network

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildGatewaySingBoxConfig(t *testing.T) {
	outbound := json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080}`)
	cfg, err := buildGatewaySingBoxConfig(outbound, "10.0.0.1", "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it's valid JSON
	var parsed map[string]any
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Check top-level keys
	for _, key := range []string{"log", "dns", "inbounds", "outbounds", "route"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}

	// Verify route has final = "proxy-out"
	route, _ := parsed["route"].(map[string]any)
	if final, _ := route["final"].(string); final != "proxy-out" {
		t.Errorf("route.final = %q, want %q", final, "proxy-out")
	}

	// Verify outbounds contains two entries (proxy-out + direct)
	outbounds, _ := parsed["outbounds"].([]any)
	if outbounds == nil {
		t.Fatal("outbounds is not an array")
	}
	if len(outbounds) != 2 {
		t.Errorf("outbounds has %d entries, want 2", len(outbounds))
	}

	// Verify DNS servers point to expected DNS
	dns, _ := parsed["dns"].(map[string]any)
	servers, _ := dns["servers"].([]any)
	if servers == nil || len(servers) != 1 {
		t.Fatal("expected exactly 1 DNS server")
	}
	srv := servers[0].(map[string]any)
	if srv["server"] != "10.0.0.1" {
		t.Errorf("dns server = %q, want %q", srv["server"], "10.0.0.1")
	}
}

func TestBuildGatewaySingBoxConfig_DefaultDNS(t *testing.T) {
	outbound := json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080}`)
	cfg, err := buildGatewaySingBoxConfig(outbound, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	dns, _ := parsed["dns"].(map[string]any)
	servers, _ := dns["servers"].([]any)
	srv := servers[0].(map[string]any)
	if srv["server"] != "1.1.1.1" {
		t.Errorf("dns server = %q, want %q", srv["server"], "1.1.1.1")
	}
}

func TestBuildGatewaySingBoxConfig_OutboundOverride(t *testing.T) {
	base := json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080}`)
	override := json.RawMessage(`{"type":"trojan","server":"5.6.7.8","server_port":443,"password":"secret","dns_server":"9.9.9.9"}`)
	cfg, err := BuildGatewaySingBoxConfig(base, override, "1.1.1.1", "5.6.7.8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	dns := parsed["dns"].(map[string]any)
	servers := dns["servers"].([]any)
	if got := servers[0].(map[string]any)["server"]; got != "9.9.9.9" {
		t.Fatalf("dns server = %v, want 9.9.9.9", got)
	}

	outbounds := parsed["outbounds"].([]any)
	first := outbounds[0].(map[string]any)
	if got := first["type"]; got != "trojan" {
		t.Fatalf("first outbound type = %v, want trojan", got)
	}
	if got := first["tag"]; got != "proxy-out" {
		t.Fatalf("override outbound must still be tagged proxy-out, got %v", got)
	}
}

func TestBuildGatewaySingBoxConfig_FullConfigDoesNotRequireProxyOut(t *testing.T) {
	base := json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080}`)
	full := json.RawMessage(`{
		"log": {"level": "debug"},
		"inbounds": [
			{"type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30"], "auto_route": true}
		],
		"outbounds": [
			{"type": "socks", "tag": "custom-out", "server": "8.8.8.8", "server_port": 1080}
		],
		"route": {"final": "custom-out", "rules": []}
	}`)
	cfg, err := BuildGatewaySingBoxConfig(base, full, "1.1.1.1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	route := parsed["route"].(map[string]any)
	if got := route["final"]; got != "custom-out" {
		t.Fatalf("route.final = %v, want custom-out", got)
	}
	if got := route["auto_detect_interface"]; got != true {
		t.Fatalf("auto_detect_interface = %v, want true", got)
	}

	outbounds := parsed["outbounds"].([]any)
	if len(outbounds) != 2 {
		t.Fatalf("outbounds length = %d, want custom-out + injected direct", len(outbounds))
	}
	if hasProxyOut := hasOutboundTag(outbounds, "proxy-out"); hasProxyOut {
		t.Fatal("full custom config should not inject proxy-out")
	}

	rules := route["rules"].([]any)
	if len(rules) < 2 {
		t.Fatalf("expected injected safety rules, got %v", rules)
	}
}

func TestBuildGatewaySingBoxConfig_FullConfigRequiresTunInbound(t *testing.T) {
	base := json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080}`)
	full := json.RawMessage(`{
		"inbounds": [{"type": "mixed", "tag": "mixed-in"}],
		"outbounds": [{"type": "direct", "tag": "direct"}],
		"route": {"final": "direct"}
	}`)
	_, err := BuildGatewaySingBoxConfig(base, full, "1.1.1.1", "")
	if err == nil {
		t.Fatal("expected error for full config without tun inbound")
	}
}

func TestBuildGatewayProxyOutbound(t *testing.T) {
	userConfig := json.RawMessage(`{"type":"socks","server":"example.com","server_port":1080,"dns_server":"10.0.0.1","bind_interface":"eth0"}`)
	out, err := buildGatewayProxyOutbound(userConfig, "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// dns_server should be removed
	if _, ok := parsed["dns_server"]; ok {
		t.Error("dns_server should have been removed")
	}
	// bind_interface should be removed
	if _, ok := parsed["bind_interface"]; ok {
		t.Error("bind_interface should have been removed")
	}
	// tag should be set to proxy-out
	if tag, _ := parsed["tag"].(string); tag != "proxy-out" {
		t.Errorf("tag = %q, want %q", tag, "proxy-out")
	}
	// server should be replaced with resolved IP
	if server, _ := parsed["server"].(string); server != "1.2.3.4" {
		t.Errorf("server = %q, want %q", server, "1.2.3.4")
	}
}

func TestBuildGatewayProxyOutbound_RealityWithUTLS(t *testing.T) {
	userConfig := json.RawMessage(`{
		"type":"vless",
		"server":"example.com",
		"server_port":443,
		"tls":{
			"enabled":true,
			"reality":{
				"enabled":true,
				"public_key":"test-key"
			}
		}
	}`)

	out, err := buildGatewayProxyOutbound(userConfig, "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Check that utls was added to tls config
	tls, ok := parsed["tls"].(map[string]any)
	if !ok {
		t.Fatal("tls config missing")
	}
	utls, ok := tls["utls"].(map[string]any)
	if !ok {
		t.Fatal("utls should have been added for reality")
	}
	if enabled, _ := utls["enabled"].(bool); !enabled {
		t.Error("utls.enabled should be true")
	}
	if fp, _ := utls["fingerprint"].(string); fp != "chrome" {
		t.Errorf("utls.fingerprint = %q, want %q", fp, "chrome")
	}
}

func TestBuildGatewayProxyOutbound_RealityWithExistingUTLS(t *testing.T) {
	// When utls is already configured, it should not be overwritten
	userConfig := json.RawMessage(`{
		"type":"vless",
		"server":"example.com",
		"server_port":443,
		"tls":{
			"enabled":true,
			"reality":{
				"enabled":true,
				"public_key":"test-key"
			},
			"utls":{
				"enabled":true,
				"fingerprint":"firefox"
			}
		}
	}`)

	out, err := buildGatewayProxyOutbound(userConfig, "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	tls, _ := parsed["tls"].(map[string]any)
	utls, _ := tls["utls"].(map[string]any)
	if fp, _ := utls["fingerprint"].(string); fp != "firefox" {
		t.Errorf("utls.fingerprint should remain 'firefox', got %q", fp)
	}
}

func TestBuildGatewayProxyOutbound_NoTLS(t *testing.T) {
	// Config without TLS should not cause issues
	userConfig := json.RawMessage(`{"type":"socks","server":"example.com","server_port":1080}`)
	out, err := buildGatewayProxyOutbound(userConfig, "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if tag, _ := parsed["tag"].(string); tag != "proxy-out" {
		t.Errorf("tag = %q, want %q", tag, "proxy-out")
	}
}

func TestBuildGatewayProxyOutbound_ResolvedIPEmpty(t *testing.T) {
	// When resolvedIP is empty, the original server should be kept
	userConfig := json.RawMessage(`{"type":"socks","server":"proxy.example.com","server_port":1080}`)
	out, err := buildGatewayProxyOutbound(userConfig, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	// Server should remain unchanged when resolvedIP is empty
	if server, _ := parsed["server"].(string); server != "proxy.example.com" {
		t.Errorf("server = %q, want %q", server, "proxy.example.com")
	}
}

func TestBuildGatewayProxyOutbound_TLSNoReality(t *testing.T) {
	// TLS without reality should not trigger utls addition
	userConfig := json.RawMessage(`{
		"type":"vless",
		"server":"example.com",
		"server_port":443,
		"tls":{
			"enabled":true,
			"server_name":"example.com"
		}
	}`)

	out, err := buildGatewayProxyOutbound(userConfig, "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	tls, _ := parsed["tls"].(map[string]any)
	// utls should NOT have been added since reality is not enabled
	if _, ok := tls["utls"]; ok {
		t.Error("utls should not have been added when reality is not present")
	}
}

func TestBuildGatewayProxyOutbound_RealityDisabled(t *testing.T) {
	// TLS with reality present but disabled should not trigger utls addition
	userConfig := json.RawMessage(`{
		"type":"vless",
		"server":"example.com",
		"server_port":443,
		"tls":{
			"enabled":true,
			"reality":{
				"enabled":false,
				"public_key":"test-key"
			}
		}
	}`)

	out, err := buildGatewayProxyOutbound(userConfig, "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	tls, _ := parsed["tls"].(map[string]any)
	if _, ok := tls["utls"]; ok {
		t.Error("utls should not have been added when reality is disabled")
	}
}

func TestBuildGatewaySingBoxConfig_ProxyServerIPInRoute(t *testing.T) {
	outbound := json.RawMessage(`{"type":"socks","server":"1.2.3.4","server_port":1080}`)
	cfg, err := buildGatewaySingBoxConfig(outbound, "10.0.0.1", "1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The route rules should include a direct bypass for the proxy server IP
	cfgStr := string(cfg)
	if !strings.Contains(cfgStr, "1.2.3.4/32") {
		t.Error("route rules should contain proxy server IP /32 bypass")
	}
}

func TestBuildGatewaySingBoxConfig_InvalidOutbound(t *testing.T) {
	// Invalid JSON in outbound should cause buildGatewayProxyOutbound to fail
	outbound := json.RawMessage(`{invalid`)
	_, err := buildGatewaySingBoxConfig(outbound, "10.0.0.1", "1.2.3.4")
	if err == nil {
		t.Fatal("expected error for invalid outbound JSON")
	}
}
