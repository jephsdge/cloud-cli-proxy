# Gateway sing-box Configuration Scenarios

This document lists common gateway sing-box configuration scenarios in TUN mode,
including symptoms, checks, root causes, and fixes. The target traffic path is:

```text
worker container
  -> cloudproxy-net-* Docker bridge
  -> cloudproxy-gw-* gateway container
  -> sing-box TUN
  -> first-hop proxy outbound, such as VLESS/Reality
  -> optional second-hop HTTP/SOCKS proxy
  -> target site
```

## Quick Triage Order

When a failure happens, separate the layers first:

```bash
gw=$(docker ps --format '{{.Names}}' | grep '^cloudproxy-gw-' | head -n 1)

docker exec "$gw" sh -lc '
echo "=== gateway interfaces ==="
ip -br addr
echo "=== gateway routes ==="
ip route
echo "=== first-hop route ==="
ip route get 183.60.217.134 || true
'

docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-' || true
```

Check these points:

- Whether DNS packets are captured by `hijack-dns`.
- Whether the first-hop proxy IP is routed into `tun0 table 2022`.
- Whether the gateway egress interface is `eth0` or `eth1`.
- Whether the current gateway uses the newer `cloudproxy-egress-*` network.
- Whether the host egress path uses a lower-MTU tunnel such as GRE/VXLAN.

## Scenario 1: DNS Is Not Hijacked, or UDP Goes to proxy

### Symptom

Gateway logs show:

```text
router: sniffed packet protocol: dns
router: UDP is not supported by outbound: proxy
```

or DNS packets are routed like normal traffic, without:

```text
router: match[...] protocol=dns => hijack-dns
```

### Cause

The DNS hijack rules are missing or placed too late. A generic route rule, such
as `final: proxy` or an `ip_is_private/direct` rule, handles the UDP DNS packet
before sing-box can hijack it.

### Fix

Place DNS hijack rules before generic route rules:

```json
{
  "action": "hijack-dns",
  "protocol": "dns"
},
{
  "action": "hijack-dns",
  "network": "udp",
  "port": 53
},
{
  "action": "route",
  "ip_is_private": true,
  "outbound": "direct"
}
```

Expected logs:

```text
router: match[...] protocol=dns => hijack-dns
dns: exchange <domain>. IN A
```

## Scenario 2: Domestic Domains Use local DNS

### Symptom

For Baidu:

```text
dns: exchange www.baidu.com. IN A
dns: match[...] rule_set=[... geosite-geolocation-cn] => route(local)
dns: exchanged A www.a.shifen.com. ... 180.101.49.44
router: match[...] rule_set=[... geosite-geolocation-cn] => route(direct)
```

### Explanation

This is expected when domestic domains are routed to local DNS:

```json
{
  "action": "route",
  "rule_set": [
    "geosite-private",
    "geosite-apple",
    "geosite-microsoft",
    "geosite-geolocation-cn"
  ],
  "server": "local"
}
```

`local` is usually:

```json
{
  "tag": "local",
  "type": "udp",
  "server": "223.5.5.5"
}
```

Meaning:

```text
www.baidu.com -> local DNS, such as 223.5.5.5 -> China IP -> direct outbound
```

## Scenario 3: Foreign Domains Should Use remote DNS

### Symptom

Google/OpenAI/Youtube/Telegram domains should hit remote DNS and proxy routing:

```text
dns: exchange www.google.com. IN A
dns: match[...] rule_set=[geosite-google ...] => route(remote)
router: match[...] rule_set=[geosite-google ...] => route(proxy)
```

If a later connection log only says:

```text
router: found reserve mapped domain: www.google.com
```

that only means `reverse_mapping` remembered a previous IP-to-domain mapping. It
does not prove which DNS server handled this query. Check the earlier
`dns: exchange` log.

### Fix

Ensure foreign domain rule sets route to `remote`:

```json
{
  "action": "route",
  "rule_set": [
    "geosite-openai",
    "geosite-google",
    "geosite-youtube",
    "geosite-telegram",
    "geosite-geolocation-!cn"
  ],
  "server": "remote"
}
```

`remote` can be DoH through the proxy:

```json
{
  "tag": "remote",
  "type": "https",
  "server": "1.1.1.1",
  "detour": "proxy"
}
```

## Scenario 4: First-Hop VLESS Connection Times Out

### Symptom

Logs:

```text
dns: lookup domain cdn.hk2.cocoduck.org
dns: lookup succeed for cdn.hk2.cocoduck.org: 183.60.217.134
outbound/urltest[香港自动选择]: dial tcp 183.60.217.134:30086: connect: connection timed out
```

### Check

Inside the gateway container:

```bash
docker exec "$gw" sh -lc 'ip route get 183.60.217.134'
```

Bad result:

```text
183.60.217.134 via 172.19.0.2 dev tun0 table 2022 src 172.19.0.1
```

This means the gateway's own first-hop proxy connection is captured by sing-box
TUN auto_route and sent back into `tun0`.

### Fix

Bind first-hop VLESS/Reality outbounds to the actual gateway egress interface:

```json
{
  "tag": "香港 2",
  "type": "vless",
  "server": "cdn.hk2.cocoduck.org",
  "server_port": 30086,
  "domain_resolver": "local",
  "bind_interface": "eth1"
}
```

Also enable:

```json
{
  "route": {
    "auto_detect_interface": true
  }
}
```

Apply `bind_interface` only to outbounds that directly dial the external
first-hop proxy. Second-hop HTTP/SOCKS outbounds with `detour` usually do not
need it.

## Scenario 5: eth0 and eth1 Differ Between Machines

### Symptom

One machine has:

```text
eth0 = 10.99.126.2/24
eth1 = 172.17.0.2/16
default via 172.17.0.1 dev eth1
```

Another machine may have:

```text
eth0 = 172.21.0.2/16
eth1 = 10.99.190.2/24
default via 172.21.0.1 dev eth0
```

### Cause

Docker names container interfaces by network attach order. `eth0` and `eth1`
do not have stable meanings.

Old layout:

```text
eth0 = cloudproxy-net-*      # worker/gateway isolated network
eth1 = docker bridge         # gateway egress
```

New layout:

```text
eth0 = cloudproxy-egress-*   # gateway egress
eth1 = cloudproxy-net-*      # worker/gateway isolated network
```

### Fix

Do not copy `bind_interface` across machines. Always check the default route:

```bash
docker exec "$gw" sh -lc 'ip route | grep "^default"'
```

If it says:

```text
default via 172.17.0.1 dev eth1
```

bind to `eth1`.

If it says:

```text
default via 172.21.0.1 dev eth0
```

bind to `eth0`.

## Scenario 6: Validate Reachability After Binding an Interface

### Commands

```bash
docker exec "$gw" sh -lc '
ip route get 183.60.217.134 oif eth1
curl --interface eth1 -v --connect-timeout 5 --max-time 8 telnet://183.60.217.134:30086 </dev/null
'
```

### Interpretation

This means TCP connect succeeded:

```text
* Connected to 183.60.217.134 (...) port 30086
...
curl: (28) Time-out
```

The final timeout is normal because curl opened a TCP connection but did not
send the VLESS/TLS/Reality handshake.

If there is no `Connected` line and the result is:

```text
connect: connection timed out
```

the egress network, host NAT, upstream path, or endpoint reachability is still
broken.

## Scenario 7: Whether MTU 1400 Is Needed

### Check the Host

```bash
ip link show
ip route
ip route get 8.8.8.8
ip route get 183.60.217.134
```

A GRE/NAT tunnel path may show:

```text
183.60.217.134 dev natgre_... src ... mtu 1476
```

On such hosts, Docker bridge MTU 1500 may cause path MTU issues. Set:

```bash
CLOUD_CLI_PROXY_NETWORK_MTU=1400
```

This is a runtime environment variable for `control-plane`, not a build
argument. It can be passed before `docker compose up`, or written to the project
root `.env` file:

```env
CLOUD_CLI_PROXY_NETWORK_MTU=1400
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE=172.30.0.0/16
```

As long as `docker-compose.yml` injects it into `control-plane`:

```yaml
CLOUD_CLI_PROXY_NETWORK_MTU: ${CLOUD_CLI_PROXY_NETWORK_MTU:-}
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE: ${CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE:-}
```

it takes effect when control-plane later runs `docker network create`. The build
step does not need this variable.

Recreate control-plane:

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yaml up -d --no-deps --force-recreate --no-build --pull never control-plane
```

Then recreate the managed host/gateway. This variable only affects newly
created `cloudproxy-net-*` / `cloudproxy-egress-*` networks. It does not modify
existing Docker networks.

If the host uses a normal MTU-1500 egress interface:

```text
default via ... dev eth0
eth0 mtu 1500
docker0 mtu 1500
br-* mtu 1500
```

MTU is usually not the first suspect. Check TUN self-capture and
`bind_interface` first.

### Verify Docker Network MTU

```bash
docker compose exec control-plane sh -lc 'echo CLOUD_CLI_PROXY_NETWORK_MTU=$CLOUD_CLI_PROXY_NETWORK_MTU'

for n in $(docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-'); do
  docker network inspect "$n" --format '{{.Name}} {{json .Options}}'
done
```

Expected:

```text
cloudproxy-net-... {"com.docker.network.driver.mtu":"1400"}
cloudproxy-egress-... {"com.docker.network.driver.mtu":"1400"}
```

It is not enough for only `cloudproxy-net-*` to have MTU 1400. The gateway's own
first-hop egress path must also use the right egress network.

## Scenario 8: cloudproxy-net Exists but cloudproxy-egress Does Not

### Symptom

```bash
docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-'
```

Only shows:

```text
cloudproxy-net-...
```

Gateway may look like:

```text
eth0 = 10.99.x.2/24
eth1 = 172.17.x.x/16
default via 172.17.0.1 dev eth1
```

### Cause

The gateway is still using the old network layout and was not recreated with
the newer `cloudproxy-egress-*` structure.

### Fix

Short term: bind first-hop outbounds to the current default-route interface,
such as `eth1`.

Long term: upgrade control-plane and recreate the managed host/gateway so both
networks are created:

```text
cloudproxy-egress-*
cloudproxy-net-*
```

## Scenario 8.1: cloudproxy-egress Subnet Conflicts With the TUN Address

### Symptom

Inside the gateway:

```text
eth0 = 172.19.0.2/16
tun0 = 172.19.0.1/30
default via 172.19.0.1 dev eth0
```

First-hop checks fail:

```text
dial tcp 54.46.4.28:30086: connect: no route to host
```

or:

```text
curl --interface eth0 telnet://54.46.4.28:30086
connect timeout
```

### Cause

Docker automatically chose `172.19.0.0/16` for `cloudproxy-egress-*`, while
sing-box TUN also uses `172.19.0.1/30`. The gateway now has the same address
range on the Docker egress interface and on `tun0`, so routing becomes
ambiguous.

### Fix

Use a control-plane version that creates `cloudproxy-egress-*` with an explicit
subnet. By default, it derives per-host subnets from:

```env
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE=172.30.0.0/16
```

If `172.30.0.0/16` conflicts with the host environment, set another IPv4 `/16`
in `.env`, for example:

```env
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE=172.31.0.0/16
```

Existing Docker networks are immutable. After changing this, rebuild/recreate
the control-plane and recreate the affected managed host/gateway so Docker
creates a new `cloudproxy-egress-*` network.

## Scenario 9: Remote Rule-Set Download Fails

### Symptom

Startup fails with:

```text
FATAL start service: initialize rule-set...
Get "https://cdn.jsdelivr.net/...": lookup cdn.jsdelivr.net: context deadline exceeded
```

or:

```text
router: initialize rule-set take too much time to finish
```

### Cause

sing-box downloads remote rule sets during startup. DNS and proxy may not be
usable yet. If `download_detour`, `default_domain_resolver`, or local DNS is not
reachable, startup can hang or fail.

### Fix

Choose by environment:

- If jsdelivr is directly reachable, route `jsdelivr.net` DNS to local.
- For foreign rule sets that need proxy, use `download_detour: proxy`.
- For private/cn rule sets, direct may be better.
- If startup proxy is unstable, prefer cached, embedded, or local rule sets.

Example:

```json
{
  "action": "route",
  "server": "local",
  "domain_suffix": [
    "jsdelivr.net"
  ]
}
```

## Scenario 10: What 1.1.1.1:443 Means

### Explanation

Logs like:

```text
outbound/...: outbound connection to 1.1.1.1:443
```

usually come from remote DoH:

```json
{
  "tag": "remote",
  "type": "https",
  "server": "1.1.1.1",
  "detour": "proxy"
}
```

Meaning:

```text
DNS query -> Cloudflare DoH 1.1.1.1:443 -> sent through proxy detour
```

This is not the target website connection; it is DNS over HTTPS.

## Scenario 11: local DNS Does Not Always Mean Host eth0

### Explanation

This DNS server:

```json
{
  "tag": "local",
  "type": "udp",
  "server": "223.5.5.5"
}
```

only means sing-box sends the DNS query to `223.5.5.5:53`. The actual interface
is still decided by gateway routing and TUN auto_route.

Check:

```bash
docker exec "$gw" sh -lc '
ip route get 223.5.5.5
ip route get 223.5.5.5 oif eth0
ip route get 223.5.5.5 oif eth1
'
```

If needed, exclude common DNS addresses from the TUN route:

```json
{
  "route_exclude_address": [
    "223.5.5.5/32",
    "119.29.29.29/32",
    "1.1.1.1/32"
  ]
}
```

## Scenario 12: What to Put in route_exclude_address

### Recommendation

Common entries:

```json
[
  "10.10.10.10/32",
  "10.10.10.20/32",
  "223.5.5.5/32",
  "119.29.29.29/32",
  "1.1.1.1/32",
  "10.99.0.0/16",
  "172.17.0.0/16"
]
```

Meaning:

- Company DNS or Docker inherited DNS.
- Local DNS and bootstrap DNS.
- Worker/gateway isolated subnet.
- Docker default bridge subnet.

Do not rely on excluding individual VLESS CDN IPs such as
`183.60.217.134/32`. CDN IPs change. Use `bind_interface` for first-hop proxy
outbounds.

## Scenario 13: Config Changed but Behavior Did Not

### Common Causes

- The sing-box config was applied in the panel, but the gateway container was
  not recreated.
- control-plane is still running an old image.
- `docker compose up` pulled remote `latest` due to `pull_policy: always`,
  overwriting the locally built image.
- Old gateway networks still exist and `cloudproxy-egress-*` was not created.

### Checks

```bash
docker compose exec control-plane sh -lc 'echo CLOUD_CLI_PROXY_NETWORK_MTU=$CLOUD_CLI_PROXY_NETWORK_MTU'
docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-'
docker inspect "$gw" --format '{{.Image}} {{.Created}}'
```

If using locally built images, avoid pulling:

```bash
CLOUD_CLI_PROXY_NETWORK_MTU=1400 docker compose -f docker-compose.yml -f docker-compose.build.yaml up -d --no-deps --force-recreate --no-build --pull never control-plane
```

After recreating control-plane, recreate the managed host/gateway so the new
network layout takes effect.

## Recommended Long-Term Fix

Do not require users to maintain `eth0` or `eth1` manually.

The control plane should:

1. Keep a stable network attach order: create and attach `cloudproxy-egress-*`
   first, then attach `cloudproxy-net-*`.
2. After gateway creation, inspect the gateway default route:

   ```bash
   docker exec "$gw" sh -lc 'ip route show default'
   ```

3. Parse the default egress interface.
4. Inject `bind_interface` into first-hop proxy outbounds.
5. Leave second-hop outbounds with `detour` unbound unless they directly dial an
   external server.

This makes the configuration independent of Docker-assigned interface names and
works across old and new gateway network layouts.
