# Gateway sing-box 配置场景问题

本文整理 gateway sing-box 在 TUN 模式下常见的配置场景问题、判断方法和处理方案。适用链路：

```text
worker 容器
  -> cloudproxy-net-* Docker bridge
  -> cloudproxy-gw-* gateway 容器
  -> sing-box TUN
  -> 第一跳代理 outbound，例如 VLESS/Reality
  -> 可选第二跳 HTTP/SOCKS 代理
  -> 目标网站
```

## 快速判断顺序

遇到失败时，先按这个顺序判断，避免把问题混在一起：

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

判断重点：

- DNS 是否被 `hijack-dns` 捕获。
- 第一跳代理节点 IP 是否被路由进 `tun0 table 2022`。
- gateway 默认出口接口到底是 `eth0` 还是 `eth1`。
- 当前 gateway 是否已经使用新的 `cloudproxy-egress-*` 结构。
- 宿主机是否存在 GRE/VXLAN 等低 MTU 出口。

## 场景 1：DNS 请求没有被 hijack，或者 UDP 被发到 proxy

### 现象

日志里出现：

```text
router: sniffed packet protocol: dns
router: UDP is not supported by outbound: proxy
```

或者 DNS 包直接按普通流量进入 `proxy`，没有出现：

```text
router: match[...] protocol=dns => hijack-dns
```

### 原因

DNS hijack 规则缺失，或者顺序太靠后，被其它路由规则提前命中。例如 `final: proxy` 或某个 `ip_is_private/direct` 规则先处理了 UDP DNS 包。

### 处理

在 `route.rules` 前部加入 DNS hijack，并放在泛化规则之前：

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

验证日志应出现：

```text
router: match[...] protocol=dns => hijack-dns
dns: exchange <domain>. IN A
```

## 场景 2：国内域名 DNS 走 local

### 现象

访问百度时看到：

```text
dns: exchange www.baidu.com. IN A
dns: match[...] rule_set=[... geosite-geolocation-cn] => route(local)
dns: exchanged A www.a.shifen.com. ... 180.101.49.44
router: match[...] rule_set=[... geosite-geolocation-cn] => route(direct)
```

### 说明

这是预期行为。配置中通常会让国内域名走 local DNS：

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

`local` 通常是：

```json
{
  "tag": "local",
  "type": "udp",
  "server": "223.5.5.5"
}
```

含义是：

```text
www.baidu.com -> local DNS，例如 223.5.5.5 -> 国内 IP -> direct 出站
```

## 场景 3：国外域名 DNS 应走 remote

### 现象

Google/OpenAI/Youtube/Telegram 等域名应命中 remote DNS 和 proxy 路由：

```text
dns: exchange www.google.com. IN A
dns: match[...] rule_set=[geosite-google ...] => route(remote)
router: match[...] rule_set=[geosite-google ...] => route(proxy)
```

如果连接日志只有：

```text
router: found reserve mapped domain: www.google.com
```

这只能说明 `reverse_mapping` 记住了之前的 IP 到域名映射，不能单独判断这次 DNS 是谁解析的。需要看更早的 `dns: exchange` 日志。

### 处理

确认 DNS 规则里国外域名指向 `remote`：

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

`remote` 可以是 DoH，并通过代理 detour：

```json
{
  "tag": "remote",
  "type": "https",
  "server": "1.1.1.1",
  "detour": "proxy"
}
```

## 场景 4：第一跳 VLESS 连接超时

### 现象

日志类似：

```text
dns: lookup domain cdn.hk2.cocoduck.org
dns: lookup succeed for cdn.hk2.cocoduck.org: 183.60.217.134
outbound/urltest[香港自动选择]: dial tcp 183.60.217.134:30086: connect: connection timed out
```

### 判断

在 gateway 容器里看第一跳 CDN IP 的普通路由：

```bash
docker exec "$gw" sh -lc 'ip route get 183.60.217.134'
```

如果看到：

```text
183.60.217.134 via 172.19.0.2 dev tun0 table 2022 src 172.19.0.1
```

说明 gateway 自己拨第一跳代理节点的连接被 sing-box TUN auto_route 捕获，又送回 `tun0`，形成自捕获。

### 处理

第一跳 VLESS/Reality outbound 需要绑定 gateway 的真实出口接口：

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

同时开启：

```json
{
  "route": {
    "auto_detect_interface": true
  }
}
```

`bind_interface` 只应该加在直接拨公网第一跳的 outbound 上。带 `detour` 的第二跳 HTTP/SOCKS 通常不需要绑定，因为它是通过第一跳访问的。

## 场景 5：`eth0` 和 `eth1` 在不同机器上不一致

### 现象

一台机器上 gateway 是：

```text
eth0 = 10.99.126.2/24
eth1 = 172.17.0.2/16
default via 172.17.0.1 dev eth1
```

另一台机器上可能是：

```text
eth0 = 172.21.0.2/16
eth1 = 10.99.190.2/24
default via 172.21.0.1 dev eth0
```

### 原因

Docker 容器里的 `eth0/eth1` 由网络 attach 顺序决定，不代表固定语义。

旧布局通常是：

```text
eth0 = cloudproxy-net-*      # worker/gateway 隔离网
eth1 = docker bridge         # gateway 出口
```

新布局通常是：

```text
eth0 = cloudproxy-egress-*   # gateway 出口
eth1 = cloudproxy-net-*      # worker/gateway 隔离网
```

### 处理

不要跨机器复制 `bind_interface`。每台机器按默认路由判断：

```bash
docker exec "$gw" sh -lc 'ip route | grep "^default"'
```

如果是：

```text
default via 172.17.0.1 dev eth1
```

就绑定 `eth1`。

如果是：

```text
default via 172.21.0.1 dev eth0
```

就绑定 `eth0`。

## 场景 6：绑定接口后如何确认底层网络可达

### 判断命令

```bash
docker exec "$gw" sh -lc '
ip route get 183.60.217.134 oif eth1
curl --interface eth1 -v --connect-timeout 5 --max-time 8 telnet://183.60.217.134:30086 </dev/null
'
```

### 结果解释

如果看到：

```text
* Connected to 183.60.217.134 (...) port 30086
...
curl: (28) Time-out
```

这表示 TCP 已经连通。最后的 `Time-out` 是正常的，因为 curl 只打开 TCP 连接，没有发送 VLESS/TLS/Reality 握手。

如果连 `Connected` 都没有，而是：

```text
connect: connection timed out
```

才说明底层 egress 网络、宿主机 NAT、上游链路或端口可达性仍有问题。

## 场景 7：MTU 1400 是否需要

### 判断宿主机

```bash
ip link show
ip route
ip route get 8.8.8.8
ip route get 183.60.217.134
```

如果出公网链路经过 GRE/NAT 隧道，可能看到：

```text
183.60.217.134 dev natgre_... src ... mtu 1476
```

这类机器上 Docker bridge 仍是 1500 时，可能出现路径 MTU 问题。可以设置：

```bash
CLOUD_CLI_PROXY_NETWORK_MTU=1400
```

这个变量是运行时环境变量，不是 build 参数。可以临时加在 `docker compose up`
前面，也可以写入项目根目录的 `.env`：

```env
CLOUD_CLI_PROXY_NETWORK_MTU=1400
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE=172.30.0.0/16
```

只要 `docker-compose.yml` 里把它注入到 `control-plane`：

```yaml
CLOUD_CLI_PROXY_NETWORK_MTU: ${CLOUD_CLI_PROXY_NETWORK_MTU:-}
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE: ${CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE:-}
```

它就会在 control-plane 后续执行 `docker network create` 时生效。build 阶段不需要这个变量。

重建 control-plane：

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yaml up -d --no-deps --force-recreate --no-build --pull never control-plane
```

然后重建 managed host/gateway。该变量只影响之后新建的 `cloudproxy-net-*` /
`cloudproxy-egress-*`，不会修改已经存在的 Docker 网络。

如果宿主机是普通 1500 MTU 出口：

```text
default via ... dev eth0
eth0 mtu 1500
docker0 mtu 1500
br-* mtu 1500
```

则 MTU 通常不是首要怀疑对象，应优先检查 TUN 自捕获和 `bind_interface`。

### 验证 Docker 网络 MTU

```bash
docker compose exec control-plane sh -lc 'echo CLOUD_CLI_PROXY_NETWORK_MTU=$CLOUD_CLI_PROXY_NETWORK_MTU'

for n in $(docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-'); do
  docker network inspect "$n" --format '{{.Name}} {{json .Options}}'
done
```

期望：

```text
cloudproxy-net-... {"com.docker.network.driver.mtu":"1400"}
cloudproxy-egress-... {"com.docker.network.driver.mtu":"1400"}
```

注意：只看到 `cloudproxy-net-*` MTU 1400 不够。gateway 自己拨第一跳代理节点的公网出口也要经过正确的 egress 网络。

## 场景 8：只有 `cloudproxy-net-*`，没有 `cloudproxy-egress-*`

### 现象

```bash
docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-'
```

只看到：

```text
cloudproxy-net-...
```

gateway 内部可能是：

```text
eth0 = 10.99.x.2/24
eth1 = 172.17.x.x/16
default via 172.17.0.1 dev eth1
```

### 原因

当前 gateway 还是旧结构，没有使用新代码创建的 `cloudproxy-egress-*`。

### 处理

短期按默认出口接口设置 `bind_interface`，例如默认路由是 `eth1` 就绑定 `eth1`。

长期应升级 control-plane 后重建 host/gateway，让新结构生成：

```text
cloudproxy-egress-*
cloudproxy-net-*
```

## 场景 8.1：`cloudproxy-egress` 子网和 TUN 地址冲突

### 现象

gateway 内部看到：

```text
eth0 = 172.19.0.2/16
tun0 = 172.19.0.1/30
default via 172.19.0.1 dev eth0
```

第一跳连通性检查失败：

```text
dial tcp 54.46.4.28:30086: connect: no route to host
```

或者：

```text
curl --interface eth0 telnet://54.46.4.28:30086
connect timeout
```

### 原因

Docker 自动给 `cloudproxy-egress-*` 分配了 `172.19.0.0/16`，而 sing-box
TUN 也用了 `172.19.0.1/30`。此时 gateway 的 Docker 出口接口和 `tun0`
落在同一个地址段，路由会变得不明确。

### 处理

升级到会显式创建 `cloudproxy-egress-*` 子网的 control-plane。默认会基于
下面的 /16 网段为每个 host 派生独立 /24：

```env
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE=172.30.0.0/16
```

如果 `172.30.0.0/16` 和宿主环境冲突，可以在 `.env` 中换成另一个 IPv4
`/16`，例如：

```env
CLOUD_CLI_PROXY_EGRESS_NETWORK_BASE=172.31.0.0/16
```

Docker network 的 subnet 不能原地修改。变更后需要重建 control-plane，并重建
受影响的 managed host/gateway，让 Docker 重新创建新的
`cloudproxy-egress-*` 网络。

## 场景 9：规则集下载失败

### 现象

启动时报：

```text
FATAL start service: initialize rule-set...
Get "https://cdn.jsdelivr.net/...": lookup cdn.jsdelivr.net: context deadline exceeded
```

或者：

```text
router: initialize rule-set take too much time to finish
```

### 原因

sing-box 启动时需要下载 remote rule-set。此时 DNS 和 proxy 可能还没有完全可用，如果 `download_detour`、`default_domain_resolver` 或本地 DNS 不可达，就会卡住。

### 处理

按实际网络环境选择：

- 国内可直连 jsdelivr 时，给 `jsdelivr.net` 加 local DNS 规则。
- 国外规则集需要代理时，`download_detour` 使用 `proxy`。
- 国内规则集或 private/cn 规则集可用 `direct`。
- 如果启动阶段代理还不稳定，优先使用缓存或内置/本地规则集，减少启动时远程下载依赖。

示例：

```json
{
  "action": "route",
  "server": "local",
  "domain_suffix": [
    "jsdelivr.net"
  ]
}
```

## 场景 10：`1.1.1.1:443` 是什么

### 说明

日志里的：

```text
outbound/...: outbound connection to 1.1.1.1:443
```

通常是 remote DNS 使用 DoH：

```json
{
  "tag": "remote",
  "type": "https",
  "server": "1.1.1.1",
  "detour": "proxy"
}
```

含义是：

```text
DNS 查询 -> Cloudflare DoH 1.1.1.1:443 -> 通过 proxy detour 发出
```

这不是目标网站连接，而是 DNS over HTTPS 连接。

## 场景 11：`local` DNS 不等于一定走宿主机 eth0

### 说明

`dns.servers` 里的：

```json
{
  "tag": "local",
  "type": "udp",
  "server": "223.5.5.5"
}
```

只表示查询发给 `223.5.5.5:53`。它最终走哪个网卡，仍由 gateway 容器的路由和 TUN auto_route 决定。

如果没有排除或绑定，local DNS 包也可能被 TUN 捕获。需要确认：

```bash
docker exec "$gw" sh -lc '
ip route get 223.5.5.5
ip route get 223.5.5.5 oif eth0
ip route get 223.5.5.5 oif eth1
'
```

必要时在 TUN inbound 上排除常用 DNS：

```json
{
  "route_exclude_address": [
    "223.5.5.5/32",
    "119.29.29.29/32",
    "1.1.1.1/32"
  ]
}
```

## 场景 12：`route_exclude_address` 适合排什么

### 建议

可以排除：

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

含义：

- 公司 DNS 或 Docker 继承的 DNS。
- local DNS 和 bootstrap DNS。
- worker/gateway 隔离网段。
- Docker 默认 bridge 网段。

不要尝试长期排除 VLESS CDN 解析出来的单个 IP。CDN IP 会变化，固定排除 `183.60.217.134/32` 这类地址不稳定。第一跳代理节点应通过 `bind_interface` 解决。

## 场景 13：gateway 配置已修改，但现象没变

### 常见原因

- 只在管理面板应用了 sing-box 配置，但没有重建 gateway 容器。
- control-plane 使用了旧镜像。
- `docker compose up` 被 `pull_policy: always` 拉了远端 latest，覆盖了本地构建镜像。
- 旧 gateway 网络还在，没有重新生成 `cloudproxy-egress-*`。

### 验证

```bash
docker compose exec control-plane sh -lc 'echo CLOUD_CLI_PROXY_NETWORK_MTU=$CLOUD_CLI_PROXY_NETWORK_MTU'
docker network ls --format '{{.Name}}' | grep -E '^cloudproxy-(net|egress)-'
docker inspect "$gw" --format '{{.Image}} {{.Created}}'
```

如果需要使用本地构建镜像，启动时避免 pull：

```bash
CLOUD_CLI_PROXY_NETWORK_MTU=1400 docker compose -f docker-compose.yml -f docker-compose.build.yaml up -d --no-deps --force-recreate --no-build --pull never control-plane
```

重建 control-plane 后，还需要重建对应 managed host/gateway，新网络结构才会生效。

## 长期修复建议

长期不要要求用户手工维护 `eth0/eth1`。

control-plane 应该：

1. 保持稳定的网络 attach 顺序：先创建并连接 `cloudproxy-egress-*`，再连接 `cloudproxy-net-*`。
2. gateway 创建后读取容器默认路由：

   ```bash
   docker exec "$gw" sh -lc 'ip route show default'
   ```

3. 解析默认出口接口。
4. 对第一跳代理 outbound 自动注入 `bind_interface`。
5. 对带 `detour` 的第二跳 outbound 保持不绑定，除非它自己直接访问外部服务器。

这样配置不依赖 Docker 给网卡分配的名字，也不会因为新旧 gateway 结构不同而需要手工改 `eth0/eth1`。
