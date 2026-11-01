# RelayDeck 技术调研

调研日期：2026-11-01。以下结论以当时可访问的官方文档与发布元数据为准；sing-box 的行为和模板能力应按实际安装版本复核。

## 官方资料

| 资料 | 链接 |
| --- | --- |
| sing-box 1.14.4 Release | <https://github.com/SagerNet/sing-box/releases/tag/v1.14.4> |
| sing-box 1.14.4 入站索引 | <https://github.com/SagerNet/sing-box/blob/v1.14.4/docs/configuration/inbound/index.md> |
| sing-box 1.14.4 出站索引 | <https://github.com/SagerNet/sing-box/blob/v1.14.4/docs/configuration/outbound/index.md> |
| sing-box 1.14.4 Endpoint 索引 | <https://github.com/SagerNet/sing-box/blob/v1.14.4/docs/configuration/endpoint/index.md> |
| sing-box TLS/Reality 配置 | <https://github.com/SagerNet/sing-box/blob/v1.14.4/docs/configuration/shared/tls.md> |
| Lucky 仓库与功能说明 | <https://github.com/gdy666/lucky> |
| Lucky 官方文档 | <https://lucky666.cn/docs/intro> |
| GOST 仓库与功能说明 | <https://github.com/go-gost/gost> |
| GOST 文档 | <https://gost.run/> |
| realm 仓库与功能说明 | <https://github.com/zhboner/realm> |

sing-box 1.14.4 的 GitHub Release 创建于 2026-10-23，发布日期为 2026-10-24（UTC），不是预发布版本。Release 中有 Linux `amd64`、`arm64`、`armv7` 资产，但没有独立 checksum 文件或签名资产。GitHub Release API 为每个归档报告 `sha256:` digest；`scripts/install-sing-box.sh` 固定并使用这三个 digest。这里能证明的是下载内容与 GitHub 记录的资产摘要一致，不能据此声称 Sing-box 项目对二进制做了加密签名。

## sing-box 1.14.4 协议矩阵

“入站/出站”表示该版本官方索引中是否存在对应配置类型；“传输”是协议本身的主要载体，具体仍由配置字段决定。Reality 是 TLS 的子配置，不是独立的本版本入站类型。RelayDeck 的 L4 规则与这些配置层是两条不同路径：前者转发密文字节，后者由 sing-box 主动发起或终止连接。

| 协议/机制 | 入站 | 出站 | 主要载体与说明 |
| --- | --- | --- | --- |
| VLESS | 是 | 是 | TCP；可配置 TLS、Reality、mux 和 V2Ray Transport |
| VMess | 是 | 是 | TCP；可配置 mux 和 V2Ray Transport |
| Trojan | 是 | 是 | TCP；可选 TLS |
| Shadowsocks | 是 | 是 | TCP/UDP；入站 `network` 可显式选择，空值通常两者 |
| ShadowTLS | 是 | 是 | TCP |
| Hysteria | 是 | 是 | QUIC/UDP |
| Hysteria2 | 是 | 是 | QUIC/UDP；入站 TLS 必填 |
| TUIC | 是 | 是 | QUIC/UDP；入站 TLS 必填 |
| AnyTLS | 是 | 是 | TCP；入站 TLS |
| Snell | 是 | 是 | 条目存在于 1.14.4 入站/出站索引；字段以该版本文档为准 |
| NaiveProxy | 是 | 是 | 入站/出站索引均列出 `naive` |
| SOCKS | 是 | 是 | 作为单跳代理或 mixed 入站使用 |
| HTTP | 是 | 是 | 作为单跳代理或 mixed 入站使用 |
| WireGuard | 否（不在入站索引） | 是 | 可作为 outbound 或 endpoint；endpoint 具有入站和出站行为 |
| SSH | 否（不在入站索引） | 是 | 出站类型 `ssh` |
| Tailscale | 否 | Endpoint | `tailscale` endpoint |
| OpenConnect | 否 | Endpoint | `openconnect` endpoint |
| OpenVPN client | 否 | Endpoint | `openvpn-client` endpoint |
| OpenVPN server | 否 | Endpoint | `openvpn-server` endpoint |
| Tor | 否 | 是 | 出站类型 `tor` |
| DNS | 否 | 是 | 出站类型 `dns` |
| Selector / URLTest | 否 | 是 | 出站选择与测速组 |
| Direct / Block / Bridge | 入站有 direct；无 block/bridge/selector 入站 | 是 | 控制、直连和桥接用途 |
| TUN / Redirect / TProxy | 是 | 否 | 系统级入站，依赖相应内核能力与路由配置 |
| Cloudflared | 是 | 否 | 入站类型 `cloudflared` |

矩阵来自上述固定 tag 的文档索引，不把“文档列出一个类型”等同于任意模板已可用。WireGuard 在此版本以 outbound/endpoint 为主；SSH 只有 outbound；它们若要作为目标连接由模板表达，仍受 RelayDeck 当前模板集和实际 sing-box 版本约束。透明 L4 规则则可以直接搬运 WireGuard 的 UDP 包或 SSH 的 TCP 字节，但它们不是由 RelayDeck 解密或终止。

## RelayDeck 的定位边界

RelayDeck 的一等能力是 L4 透明转发。它把 TCP 流或 UDP 会话中的字节原样送往目标，因此 VLESS、Reality、Hysteria2、TUIC、普通 TLS、SSH 等只要只依赖这些字节和网络语义，就不需要专门的协议适配器。优势是协议升级不必修改转发器，代价是它看不到单个用户或子连接的含义。

需要写进产品说明的限制：

- **不保留原始源 IP**。目标是普通 TCP/UDP 目标时，目标看到的是 RelayDeck 的出口地址；转发层没有自行注入 PROXY protocol。需要真实来源的协议不能依赖本功能。
- **TLS SNI 不变**。客户端 TLS ClientHello 被当作不透明字节转发，RelayDeck 不做 SNI 改写或 TLS 终止。
- **UDP 是必要条件**。QUIC、HTTP/3、Hysteria2、TUIC 需要 UDP；只建 TCP 规则不能承载它们的 QUIC 流量。
- **未来协议不保证**。新增协议可能假设原始源地址、ICMP、端口跳跃、特殊分片、内核路由或协议感知；透明转发无法承诺这些能力。
- **会话生命周期不同**。TCP 是有连接字节流，UDP 是带超时的会话映射；负载均衡、连接计数和目标选择不能把两者简单等同。
- **主动节点不是透传**。一旦交给 sing-box 出站或 endpoint，协议终止、路由、DNS 和认证语义由 sing-box 配置决定。

## 对 Lucky 的借鉴与取舍

Lucky 的官方定位是集成 DDNS、证书、Web 反向代理、TCP/UDP 端口转发、STUN、文件服务和设备自动化的综合网络工具。它验证了“在单一 Web 面板中看规则、证书和运行状态”的可用性，也说明家庭/NAS 用户需要明确备份和安全提示。其文档还明确提醒修改默认账号、限制入口路径，并说明入口路径不能替代认证或防火墙。

RelayDeck 借鉴规则列表、运行状态和集中配置的交互方向，但把范围收敛到 L4 转发与可选代理引擎。没有域名自动签发、DDNS、STUN、文件服务和设备自动化，避免在没有需求时扩展攻击面。安装器不沿用共享默认密码，也不会通过默认账号来降低首次体验；管理员密码由系统随机生成并只经 stdin 初始化。

## 对 GOST 的借鉴与取舍

GOST 支持多端口监听、多级转发链、TCP/UDP 端口转发、反向代理、多种代理协议、路由、限速和插件。它适合把代理链和协议适配都放在一个引擎中。RelayDeck 不复制插件系统或全量代理协议栈：常规公网入口保持无协议解析的 L4 路径，把主动链、节点和特殊出站交给可选 sing-box。这样主进程依赖少，规则失效时也更容易区分“转发故障”和“内核配置故障”。

借鉴点包括每个规则的启停、流量计数、连接数、错误信息，以及配置前验证。取舍是不把任意插件加载进管理服务，也不把管理面当成通用代理控制面；sing-box 的配置修改必须遵循其自身验证与进程启动结果。

## 对 realm 的借鉴与取舍

realm 的官方说明强调简单、高性能和低资源，TCP/UDP relay 是内置能力，并提供连接/超时等运维选项。它证明了极简 L4 转发可以不依赖大型反向代理。RelayDeck 采用相近的直接 TCP/UDP relay 模型、低内存运行时参数和每个规则的实时状态，再增加 Web 管理、认证、HTTPS 与显式审计信息。

RelayDeck 不宣称比 realm 更快，也不继承 realm 可选的 PROXY protocol、TLS transport、负载均衡或 hook 等全部特性。当前矩阵中这些能力如需使用，应由明确的目标侧配置或 sing-box 模板承担；透明转发本身不承诺原始源 IP 保留。

## 运维结论

低资源部署适合 `CGO_ENABLED=0` 的 Go 静态二进制、`GOMEMLIMIT=96MiB`、`GOGC=50` 和 systemd 沙箱。公网没有域名时使用 IP SAN 自签证书，浏览器首次连接必须人工确认信任；这是安全边界，不是需要隐藏的安装错误。安装器保持主机状态最小改动：不调 sysctl、不改防火墙、不卸载软件、不让更新重置认证。
