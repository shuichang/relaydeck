# 验证与支持边界

本次独立审查与四项修复记录见 [review-2026-11-01.md](review-2026-11-01.md)。

## 可重复执行的检查

```bash
go test ./...
go test -race ./...
go vet ./...
bash -n scripts/install.sh scripts/install-sing-box.sh
shellcheck scripts/install.sh scripts/install-sing-box.sh
bash scripts/test-installer.sh
```

真实协议集成测试需要 sing-box 1.14.4；普通 `go test ./...` 在未指定内核时跳过该项：

```bash
sudo bash scripts/install-sing-box.sh
RELAYDECK_SING_BOX=/usr/local/bin/sing-box go test -count=1 -v ./internal/integration
```

测试创建临时 HTTP 目标、sing-box 客户端与服务端，并在中间接入 RelayDeck 原生转发器。VLESS、VLESS + Reality 使用 TCP，Hysteria2 使用 QUIC/UDP。每条链路分别验证 IPv4 与 IPv6 入口到 IPv4 服务端，并核对 HTTP 回显内容。Reality 使用本地 TLS 诱饵服务，Hysteria2 显式信任临时证书；不需要真实节点凭据。所有进程与临时监听在测试后关闭。

CI 对分支 push、PR 和版本 tag 执行普通测试、竞态检测、静态检查、安装脚本检查与真实协议测试。通过后才为版本 tag 构建 amd64、arm64、armv7 安装包并发布 SHA-256 清单。

界面回归也已纳入 CI，使用隔离临时管理员、随机本机端口和真实 Chromium。生产程序不依赖 Node 或浏览器。开发环境可运行：

```bash
npm install --no-save --package-lock=false playwright@1.64.1
npx playwright install chromium
mkdir -p .tools
go build -o .tools/relaydeck-ui ./cmd/relaydeck
RELAYDECK_BINARY="$PWD/.tools/relaydeck-ui" node scripts/test-web.cjs
```

`RELAYDECK_CHROMIUM` 可指定已有浏览器的绝对路径，`RELAYDECK_PLAYWRIGHT` 可指定 Playwright 模块绝对路径，`RELAYDECK_UI_ARTIFACTS` 可指定截图及 JSON 结果目录。脚本只管理自己创建的临时数据和预览进程。

## 覆盖内容

- 原生 TCP、UDP 与双协议监听，IPv4/IPv6 入站到 IPv4。
- Linux UDP 通配监听保存目的地址与接口，正确回复多 IP 地址并隔离会话。
- TCP 半关闭、静默上游退出、实时流量计数与连接限制。
- 新默认上限 8192、最大配置值 65536、旧显式值保留；512 条同时保持的真实 TCP 回显连接。
- 草稿连通性预检、目标 TCP 拒绝、UDP 不误报 RTT、认证/CSRF、四请求并发限制。
- 检测与规则更新之间的端口协调、可取消等待，以及入口检测不占用业务连接配额。
- UDP 空报文、报文边界、会话限制与空闲回收。
- CIDR 白名单、监听冲突、自转发直接环路与超大参数检查。
- 配置绑定失败回滚、磁盘保存失败回滚、未改规则的会话保留。
- 完整序列化配置的大小限制、缩进膨胀与规则/内核配置合并后的容量检查。
- HTTPS 管理会话、CSRF、来源校验、登录限速、密码修改及会话撤销。
- 启动时目标解析或端口绑定失败仍保留管理入口，显示待修复规则。
- 可选内核配置检查、进程启停、崩溃恢复、启动失败回滚及日志脱敏。
- 内核跨进程周期的连续崩溃预算；稳定运行后重置退避状态。
- 十份 sing-box 配置模板的 JSON 结构及内核兼容性。
- 通用直连及 Reality/XUDP 模板经 IPv4/IPv6 入口承载普通 TCP 字节与 UDP 报文的真实回显。
- 安装器拒绝其他进程占用端口、错误版本与不稳定进程的健康响应。

## 不能由这些测试证明的内容

本地与 CI 的协议链路测试不等于每个运营商、真实节点、丢包条件或 MTU 组合的兼容性验证。未对所有路由、所有节点协议和未来版本逐一实测，也不承诺每台 VPS 的吞吐或内存峰值。

当前范围不包含端口段批量映射、内核 NAT、客户端源地址保留、跨多条规则的循环图检测、集群高可用、订阅管理、DDNS、NAT 穿透控制面和公网 CA 自动证书续期。协议内核的高级系统接口还受 systemd 沙箱权限约束；当前模板使用常规用户态连接。

## 2026-11-02 增量优化

详见 [update-2026-11-02.md](update-2026-11-02.md)。浏览器显示的 HTTP 往返包含面板请求和处理耗时，不等于转发端口或代理协议的 RTT。入口只检查本机监听或草稿端口可绑定性；目标毫秒仅代表中转机到目标 IPv4 的 TCP 握手，若目标是本机隧道入口，不能据此判断隧道远端及最终目标可用。UDP 无通用握手，必须由真实协议和双向流量验证。

在同一 Linux VPS、相同 Go 版本上保持 1500 条已完成回显的 TCP 连接，新旧实现的保留堆分别为每连接 85,692 与 20,191 字节。基准包含转发器、客户端和回显目标，不能当作生产进程 RSS、所有协议或吞吐保证。可复现：

```bash
go test ./internal/forward -run '^$' \
  -bench '^BenchmarkTCPIdleConnections$' -benchtime=1500x
```
