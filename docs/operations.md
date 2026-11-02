# RelayDeck 运维手册

目标域名会在启动或保存规则时解析为 IPv4，当前没有按 DNS TTL 定时刷新；记录变化后需要重新保存规则或重启服务。如果启动时出现解析或端口占用错误，管理面板仍会启动并显示待修复规则。转发配置变更失败会恢复旧监听，但已关闭的活动连接不能恢复。

## 安装布局

| 项目 | 路径或值 |
| --- | --- |
| 程序 | `/usr/local/bin/relaydeck` |
| 服务 | `relaydeck.service` |
| 认证与运行数据 | `/var/lib/relaydeck` |
| TLS 与配置 | `/etc/relaydeck` |
| 管理监听 | `:9443`（HTTPS） |
| 可选内核 | `/usr/local/bin/sing-box` |

认证文件是 `/var/lib/relaydeck/auth.json`，目录权限为 `0700`，文件权限为 `0600`。文件中保存用户名、随机盐和加盐哈希，不保存明文密码。安装器不把管理员密码写入配置文件、命令行参数、systemd 日志或仓库。首次安装成功时，随机密码只在 root 终端显示一次。

TLS 使用自签证书 `/etc/relaydeck/tls.crt` 和私钥 `/etc/relaydeck/tls.key`，两者权限均为 `0600`。管理入口没有绑定域名，因此浏览器默认显示证书不受信任。核对公网 IP 与证书指纹后，可以仅为该入口添加信任例外；这只是本地信任，不代表证书由公共 CA 签发。

## 安装、更新与卸载

以下命令在 root 终端执行；非 root 用户需要把 `bash` 换成 `sudo bash`：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh | bash
```

固定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh \
  | bash -s -- --version v0.1.2
```

首次创建证书时，默认优先读取本机路由及网卡上的公网 IPv4，再读取公网 IPv6；没有合适的本机地址时，按 IPv4/IPv6 使用多个 HTTPS 检测接口。外部请求有连接及总时限、绕过代理环境变量，并验证返回的确实是可用 IP 候选；单个接口失败不会直接中止安装。自动识别失败不表示服务器没有公网 IP。NAT、双线或多出口环境可显式指定入口 IP：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh \
  | env RELAYDECK_PUBLIC_IP=203.0.113.10 bash
```

请替换为当前这台机器的入口地址。IPv6 不带方括号或端口；`RELAYDECK_PUBLIC_IP` 要设置给管道右侧的 `bash`。已有完整证书和认证的更新流程不会重新探测或改写它们。

安装脚本执行以下顺序：架构识别、下载 Release 归档和 `checksums.txt`、校验 SHA-256、校验二进制及预期版本、备份上一个二进制和 systemd unit、原子替换、重新加载 systemd、启动服务并验证健康身份。健康判定要求 unit 连续保持 active、MainPID 稳定且存活、`9443` 的 listen socket 属于该 MainPID，同时本机 `https://localhost:9443/healthz` 返回 HTTP 200 且 JSON 的 `service` 为 `relaydeck`、`version` 与刚下载二进制一致。任一项失败时，脚本成对恢复上一个二进制和原 unit；首次安装失败时移除部分安装并停止服务。

重复安装会更新单元和程序，保留：

- `/var/lib/relaydeck/auth.json` 与配置；
- `/etc/relaydeck` 内的配置与 TLS 文件；
- `/var/lib/relaydeck/.relaydeck-previous`，上一版本的二进制备份；
- `/var/lib/relaydeck/.relaydeck-previous.service`，上一版本的 systemd unit 备份。

更新流程不会调用 `relaydeck init`，因此不会重置密码。自签证书存在时也不会重新生成。

安装与更新只管理 `relaydeck.service` 和本项目自己的文件。它不会停止、改动或卸载服务器上已有的其他服务。

如果管理员密码丢失，可用 root 权限显式重置。下面的密码不会出现在命令参数或 shell 历史中；`printf` 的独立管道与执行 curl 脚本的 stdin 互不复用：

```bash
sudo systemctl stop relaydeck
read -rsp 'New admin password: ' relaydeck_password
printf '\n'
printf '%s\n' "$relaydeck_password" \
  | sudo relaydeck init --data /var/lib/relaydeck --username admin \
      --password-stdin --force
unset relaydeck_password
sudo systemctl start relaydeck
```

重置后应确认服务重新通过健康检查，并在浏览器重新登录。管理员会话保存在进程内，服务重启会使既有会话失效。

卸载：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh \
  | bash -s -- --uninstall
```

卸载只停止并禁用服务，删除 `relaydeck.service` 和 `/usr/local/bin/relaydeck`。`/etc/relaydeck` 与 `/var/lib/relaydeck` 默认保留。彻底删除数据需要由管理员确认备份后手动处理；脚本不会删除用户数据或其他软件。

## 网络与端口

RelayDeck 默认监听所有接口的 `9443/TCP`。安装器不执行 `iptables`、`nft`、`ufw`、`firewalld` 或云安全组变更，也不会修改任何全局 sysctl。应自行限制管理端口的来源，并按规则开放实际需要的中转端口。

服务单元只保留 `CAP_NET_BIND_SERVICE`，允许规则监听 1024 以下端口。该单元关闭了 TUN、TProxy 等所需的其他能力；需要这些模式的主动内核配置应单独评估，不应通过放宽整个单元权限来实现。

自签证书的公网启用条件：

- 服务器的入口公网 IPv4 或 IPv6 地址已在证书的 IP SAN 中；
- 公网到 `9443/TCP` 的路由和防火墙已放通；
- 浏览器能访问该 IP，并能确认一次性信任提示。

如果更换公网 IP，需要生成包含新 IP 的证书。先确认旧证书和私钥已备份，再执行：

```bash
sudo relaydeck certificate --force \
  --cert /etc/relaydeck/tls.crt \
  --key /etc/relaydeck/tls.key \
  --hosts 203.0.113.10,localhost
sudo chmod 0600 /etc/relaydeck/tls.crt /etc/relaydeck/tls.key
sudo systemctl restart relaydeck
```

证书私钥不应交给普通用户。浏览器若仍提示旧证书，应关闭对应站点的旧例外后重新访问；不要为了绕过提示而全局关闭 TLS 校验。

`certificate` 默认有效期是 365 天，可用 `--days` 指定 1–3650 天。应在到期前安排轮换；更新程序不会自动续期自签证书。

## 日常检查

```bash
sudo systemctl status relaydeck --no-pager
sudo journalctl -u relaydeck --since "30 minutes ago"
curl -kfsS https://localhost:9443/healthz
relaydeck version
sudo relaydeck check --data /var/lib/relaydeck
```

`curl -k` 只用于本机确认服务是否响应，不应用于传播证书或关闭客户端验证。外部访问应通过浏览器接受自签例外，或导入明确的证书指纹。

安装健康检查不只看 `curl` 的 200。监听端口被其他 HTTPS 服务占用时，RelayDeck 的 MainPID 不拥有该 socket，或响应中的服务名/版本不一致，安装都会失败并回滚。安装器只停止自己的 `relaydeck.service`，不会结束占用端口的外部进程。

可用独立随机回环端口验证这些判定和 tar 管道行为。测试需要 `go`、`openssl`、`curl` 和 `ss`，使用替身 systemd 状态与现场构建的临时 Go HTTPS 服务，明确避开 `9443`：

```bash
bash scripts/test-installer.sh
```

查看证书指纹：

```bash
openssl x509 -in /etc/relaydeck/tls.crt -noout -subject -issuer -fingerprint -sha256
```

## 备份与恢复

备份时应停止写入，至少保存：

```bash
sudo systemctl stop relaydeck
sudo tar -C / -czf relaydeck-backup.tgz \
  etc/relaydeck var/lib/relaydeck
sudo systemctl start relaydeck
```

备份包含管理员认证文件和 TLS 私钥，应加密并限制访问。恢复是同架构迁移时直接还原目录，再运行安装器修复程序与单元：

```bash
sudo tar -C / -xzf relaydeck-backup.tgz
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install.sh | bash
```

安装器不会因为认证文件已存在而再次打印密码。迁移前应确认认证文件完整，且已记录对应管理员密码。

## systemd 限制与调整

安装器写入的单元启用了：

- `ProtectSystem=strict`、只允许写 `/var/lib/relaydeck`；
- `ProtectHome`、`PrivateTmp`、`PrivateDevices`、`NoNewPrivileges`；
- `ProtectKernelTunables`、`ProtectKernelModules`、`ProtectControlGroups`；
- `RestrictAddressFamilies`、`RestrictNamespaces`、`RestrictRealtime`；
- `MemoryDenyWriteExecute`、`LockPersonality`、`SystemCallArchitectures=native`；
- 系统调用过滤与单一网络绑定能力。

这些限制会随重新执行安装脚本恢复。需要调整时，使用：

```bash
sudo systemctl edit relaydeck
```

资源默认值是 `GOMEMLIMIT=96MiB` 和 `GOGC=50`。对内存更紧的机器，可在 drop-in 中覆盖后运行 `systemctl daemon-reload` 和 `systemctl restart relaydeck`；设置过低可能造成更频繁的垃圾回收。不要在命令行的 systemd 单元变更中记录任何密码。

## 可选 sing-box

安装固定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/shuichang/relaydeck/main/scripts/install-sing-box.sh \
  | sudo bash
```

脚本只写 `/usr/local/bin/sing-box`，不创建配置或服务。它固定校验下表的 SHA-256：

| 资产 | SHA-256 |
| --- | --- |
| `sing-box-1.14.4-linux-amd64.tar.gz` | `699a1b510aa4396b2176d32d1181920399ebf1e267835da6f3b598d0f7c24968` |
| `sing-box-1.14.4-linux-arm64.tar.gz` | `436441f76ee2f7fe21b5b2d426d3cb261a480455a704865789ad533e4273ec26` |
| `sing-box-1.14.4-linux-armv7.tar.gz` | `fd164ed45ebc784f657a3e1d0e0ad0f68afdcff864ee57122c2cfed3ddb80a00` |

这些摘要在 2026-11-01 从 GitHub Release API 的资产 digest 读取，并与实际发布的 1.14.4 资产名对应。该 Release 没有单独的 checksum 或签名资产，因此脚本不声称执行了上游签名验证。已经有其他版本时，脚本默认拒绝覆盖；确认升级/回退方案后才使用 `--force`，旧文件会保留为 `sing-box.relaydeck-backup`。

同时运行转发器和主动内核会增加资源消耗。协议矩阵和当前限制见 [research.md](research.md)。
