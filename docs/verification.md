# 验证与支持边界

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

## 覆盖内容

- 原生 TCP、UDP 与双协议监听，IPv4/IPv6 入站到 IPv4。
- TCP 半关闭、静默上游退出、实时流量计数与连接限制。
- UDP 空报文、报文边界、会话限制与空闲回收。
- CIDR 白名单、监听冲突、自转发直接环路与超大参数检查。
- 配置绑定失败回滚、磁盘保存失败回滚、未改规则的会话保留。
- 完整序列化配置的大小限制、缩进膨胀与规则/内核配置合并后的容量检查。
- HTTPS 管理会话、CSRF、来源校验、登录限速、密码修改及会话撤销。
- 启动时目标解析或端口绑定失败仍保留管理入口，显示待修复规则。
- 可选内核配置检查、进程启停、崩溃恢复、启动失败回滚及日志脱敏。
- 内核跨进程周期的连续崩溃预算；稳定运行后重置退避状态。
- 九份 sing-box 配置模板的 JSON 结构及内核兼容性。

## 不能由这些测试证明的内容

本地与 CI 的协议链路测试不等于每个运营商、真实节点、丢包条件或 MTU 组合的兼容性验证。未对所有路由、所有节点协议和未来版本逐一实测，也不承诺每台 VPS 的吞吐或内存峰值。

当前范围不包含端口段批量映射、内核 NAT、客户端源地址保留、跨多条规则的循环图检测、集群高可用、订阅管理、DDNS、NAT 穿透控制面和公网 CA 自动证书续期。协议内核的高级系统接口还受 systemd 沙箱权限约束；当前模板使用常规用户态连接。
