# RelayDeck

RelayDeck 是一个面向公网入口的轻量 TCP/UDP 中转面板。它工作在 L4：转发器只处理连接和字节流，不解析上层内容，因此可以直接承载 VLESS、Reality、Hysteria2、TUIC、TLS、SSH 等协议的 TCP/UDP 流量。协议自身的加密、认证和伪装仍由两端客户端/服务端完成。

RelayDeck 的原生 TCP/UDP 规则转发在进程内完成，无需启动 sing-box 内核。主动出口、代理链和隧道由可选的 sing-box 内核提供；没有安装 sing-box 时，规则转发和面板功能仍可使用。它不依赖 Nginx。

## 适用范围

- 公网 IPv4/IPv6 入口到固定目标的 TCP/UDP 端口转发。
- 对 VLESS、Reality、Hysteria2 等协议做透明字节转发，不修改 TLS 握手及 SNI。
- 通过可选 sing-box 内核配置主动节点、出口代理或隧道。
- 单机、低资源 VPS，以及需要明确端口访问控制的场景。

透明转发能传递协议字节，但不会保留客户端原始源 IP。TCP 和 UDP 上的协议加密会保留；依赖 UDP 的 QUIC/Hysteria2 必须配置 `udp` 或 `both` 规则。未来出现的所有新协议不能预先保证兼容，尤其当协议要求透明源地址、特殊 ICMP 行为、端口跳转或内核级路由时。

每条规则的 `max_connections` 省略或设为 `0` 时采用有限默认上限 `8192`，可显式设置为 `1` 至 `65536`。已有显式值会保留，包括旧配置中的 `256`；它不会因为升级而被自动放大。API 的 `effective_max_connections` 返回最终生效值。

当前 sing-box 1.14.4 的协议支持与限制见 [docs/research.md](docs/research.md)。WireGuard、SSH 等连接可以通过 L4 的 UDP/TCP 规则承载；主动 WireGuard/SSH 出口依赖引擎模板，模板能力受实际安装的内核版本约束。

测试方法与明确的支持边界见 [docs/verification.md](docs/verification.md)。CI 包含真实 VLESS、VLESS + Reality 和 Hysteria2 的 IPv4/IPv6 转发链路测试。

## 安装

要求 Linux、systemd、root 权限，以及 `curl`、`tar`、`sha256sum`、`ss`（Debian 的 `iproute2` 包）。安装器支持 `amd64`、`arm64` 和 `armv7`。它从 GitHub Release 下载静态 Go 二进制，并按同一 Release 中的 `checksums.txt` 校验 SHA-256。

公开仓库：<https://github.com/shuichang/relaydeck>

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh | bash
```

以上命令在 root 终端执行。非 root 用户才需要把 `bash` 换成 `sudo bash`，下面的参数形式也应相应写成 `sudo bash -s -- ...`。

安装最新 Release。需要固定版本时，在安装脚本后传参：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh \
  | bash -s -- --version v0.1.5
```

首次安装会：

- 安装 `/usr/local/bin/relaydeck`，创建 `relaydeck.service`。
- 优先读取本机路由及网卡上的公网 IPv4/IPv6；没有合适的本机地址时，使用多个 HTTPS 检测接口。为选定 IP 和 `localhost` 生成自签 HTTPS 证书。
- 生成随机管理员密码，通过一条独立的 `printf` 管道交给 `relaydeck init --password-stdin`。密码只在首次安装成功时向 root 终端显示一次，`auth.json` 权限为 `0600`。
- 监听 `:9443`，通过 HTTPS 提供管理面板。

没有域名时，IPv4 管理地址是 `https://服务器公网IP:9443/`，IPv6 地址要写成 `https://[公网IPv6]:9443/`。浏览器会提示证书不受信任，这是自签 IP 证书的预期行为；确认地址和证书指纹后，可仅为该站点添加信任例外。证书不会获得公共 CA 信任。检测接口不可达不代表服务器没有公网 IP；脚本会先使用合适的本机地址。若仍无法自动识别，或 NAT、多出口环境下选定 IP 与实际入口不同，请把下例地址替换成这台机器的入口 IP：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh \
  | env RELAYDECK_PUBLIC_IP=203.0.113.10 bash
```

IPv6 环境变量直接填写地址，不加方括号或端口。变量要传给管道右侧的 `bash`；只给左侧 `curl` 设置变量不会传递给安装脚本。

重复执行安装命令即为更新。更新会保留 `/var/lib/relaydeck` 中的认证和配置、`/etc/relaydeck` 中的证书与配置，不会重置管理员密码。安装器会先备份原二进制与原 systemd unit，再原子替换。启动后要连续确认 unit 持续 active、MainPID 稳定且存活、9443 的监听 socket 属于该 PID，并核对 `/healthz` 的同一服务名和版本；任一项不符都会成对恢复，避免其他同端口 HTTPS 服务造成误判。

卸载只移除程序与 systemd 单元：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh \
  | bash -s -- --uninstall
```

`/var/lib/relaydeck` 和 `/etc/relaydeck` 默认保留，便于重装或迁移。脚本不会卸载其他软件，不会修改全局 sysctl，也不会增删防火墙规则。

## 运行与安全

服务使用项目的固定命令：

```text
/usr/local/bin/relaydeck serve --data /var/lib/relaydeck --listen :9443 \
  --tls-cert /etc/relaydeck/tls.crt --tls-key /etc/relaydeck/tls.key
```

systemd 单元启用了文件系统、设备、内核、命名空间、系统调用和权限限制，只保留 `CAP_NET_BIND_SERVICE`，因此配置规则时可以监听 1024 以下端口。设置 `GOMEMLIMIT=96MiB` 与 `GOGC=50` 控制低内存环境下的 Go 运行时占用。需要调整资源上限时，应使用 systemd drop-in 覆盖环境变量。

安装器不会替你开放公网端口。请自行确认云安全组或主机防火墙允许管理端口 `9443/TCP`，以及规则实际使用的 TCP/UDP 端口。不要把管理面板直接暴露给无关来源；优先限制管理端口的来源 IP。

更多操作说明见 [docs/operations.md](docs/operations.md)。

## 面板设置与 HTTPS 证书

登录后进入“设置 → SSL 证书”，可以上传或粘贴 PEM 证书链与私钥、指定服务器上的证书/私钥路径、生成包含 IP 或域名的自签证书，或恢复启动参数指定的默认证书。证书与私钥匹配、有效期和证书链校验通过并保存成功后，新连接立即使用新证书，不需要重启面板或转发规则。

文件路径模式每 60 秒检查续期后的文件，也可手动重新载入。读取失败、证书过期或私钥尚未更新时，会继续使用上一次有效证书并显示错误；重新启动也保留这份备份。TLS 设置保存在 `/var/lib/relaydeck/tls-settings.json`，权限 `0600`，不包含在普通规则配置导出中。私钥只允许提交，不会通过设置查询接口回显。

公共 CA 证书须通过证书 SAN 中匹配的域名/IP 访问；自签证书更换后可能需要重新确认浏览器信任。当前设置页提供已有证书的配置和续期文件重载，不代办 ACME 申请。文件路径必须能被服务读取，systemd 默认隐藏 `/root` 等用户目录；可使用 `/etc/letsencrypt` 或应用数据目录下的文件。

规则列表会分别显示当前错误和带时间的最近错误记录。TCP 上游连接恢复后清除 TCP 当前错误；UDP 必须收到目标真实回复才标记恢复，两种协议的状态独立。大字号规则名称、地址、状态和检测结果方便桌面与手机查看。

## 可选 sing-box 内核

在面板的“协议内核”页面点击“一键安装 sing-box”。面板会后台下载官方 `1.14.4`，显示下载、校验、解压及版本检测状态；失败时可以重试。安装成功后无需重启面板，填写模板中的远端参数，再点击“校验并保存”或勾选启用后点击“保存并启用”。模板中的示例参数需要替换为实际节点信息，安装本身不会启动模板。

本次修复和审查记录见 [docs/core-install-2026-11-02.md](docs/core-install-2026-11-02.md)。

面板安装目录为 `/var/lib/relaydeck/engines/sing-box`（自定义数据目录时为其下的 `engines/sing-box`）。系统 PATH 中的 sing-box 优先，缺少时自动发现私有内核；不会覆盖系统安装。支持 Linux amd64、arm64 和 armv7。使用 `--engine-binary` 自定义其他路径时，请自行安装到指定位置。下载需要服务器能够通过 HTTPS 访问 GitHub 官方 Release；下载归档和解压大小均有限制，并校验固定 SHA-256。

无法从面板安装时，也可在 root 终端使用备用脚本：

`scripts/install-sing-box.sh` 只安装固定版本 `1.14.4` 的官方 Linux 二进制到 `/usr/local/bin/sing-box`，不创建或修改 RelayDeck 配置与服务：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install-sing-box.sh \
  | bash
```

三个支持架构的归档 SHA-256 已固定在脚本中，来源为 GitHub Release API 的资产 digest。上游没有独立的签名或 `checksums.txt`；该步骤是发布资产完整性校验，不应描述为上游数字签名验证。如果目标路径已有不同版本的 sing-box，脚本会拒绝覆盖，除非显式传入 `--force`。

模板列表首项 `direct-passthrough` 是通用 TCP/UDP direct 入站到 direct 出站的配置，属于可选的 sing-box 出站隧道，不会限制原生转发规则。Reality 模板可承载配置允许的 TCP 与 UDP 流量，UDP 还要求远端支持 XUDP；Reality 不会自动转换其他协议。SSH 模板仍只承载 TCP。

## 构建

Release 由 GitHub Actions 在 tag `v*` 上执行：

1. `go test ./...`、`go test -race ./...` 和 `go vet ./...`。
2. 使用 `CGO_ENABLED=0` 静态构建 `linux/amd64`、`linux/arm64`、`linux/armv7`。
3. 打包二进制、生成 `checksums.txt`，并发布到 GitHub Releases。

普通分支 push 与 Pull Request 同样运行测试、race、vet 和 shellcheck；构建与发布步骤只对 `v*` tag 生效。CI 固定使用 Go `1.27.x`（当前验证版本为 1.27.3），`go.mod` 的 Go 1.24 是最低兼容版本。

本地构建需要 Go 1.24 或更新版本：

```bash
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o relaydeck ./cmd/relaydeck
```

## 许可

[MIT](LICENSE)
