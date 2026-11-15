# RelayDeck Lite

RelayDeck Lite 是 RelayDeck 的轻量 Rust 分支，面向 128 MiB 内存、低 CPU 配额和小磁盘 LXC。它只做透明 TCP/UDP 端口直连，不解析 VLESS、Reality、XHTTP、Hysteria2 或其他上层协议，也不依赖 sing-box。

运行时使用 Tokio 单线程调度器。TCP 连接使用异步双向复制，UDP 使用按客户端地址建立的短生命周期关联。管理通过 Unix 控制 socket 完成，不启动 Web 服务。

默认每条规则最多 512 个会话（`both` 规则的 TCP 与 UDP 共享这个上限），同时受进程级 512 个 TCP 会话和 256 个 UDP 会话预算约束，避免单条规则或多条规则把 128 MiB 容器的内存耗尽。需要更高容量时应先做 cgroup RSS 和连接压力测试，再调整源码常量与服务资源限制。

## 构建

```bash
cd rust-lite
cargo build --release
strip target/release/relaydeck-lite
```

The release workflow builds both Debian/glibc and Alpine/musl binaries. The musl binary is smoke-tested inside Alpine 3.23.

## 运行

```bash
sudo install -Dm755 target/release/relaydeck-lite /usr/local/bin/relaydeck-lite
sudo install -d -m700 /etc/relaydeck-lite /var/lib/relaydeck-lite
sudo relaydeck-lite run \
  --config /etc/relaydeck-lite/rules.json \
  --socket /run/relaydeck-lite.sock
```

默认规则文件是 `/etc/relaydeck-lite/rules.json`，默认控制 socket 是 `/run/relaydeck-lite.sock`。

Debian 使用 `deploy/relaydeck-lite.service`，Alpine 使用 `deploy/relaydeck-lite.openrc`。systemd 服务文件提供文件描述符、任务数和 96 MiB 内存上限；OpenRC 文件使用 `supervise-daemon` 限制崩溃重启次数，并设置文件描述符上限。安装时把二进制放到 `/usr/local/bin/relaydeck-lite`，创建 `/etc/relaydeck-lite` 与 `/var/lib/relaydeck-lite` 后启用对应服务即可。CPU 配额由 LXC 宿主机 cgroup 提供，服务内部不重复设置 CPUQuota。首次启动即使规则 DNS 失败或监听端口冲突，Unix 控制 socket 仍会保留，可以先用 `status`、`update` 或 `delete` 修复配置，再执行 `start`。

## 终端管理

查看规则和状态：

```bash
relaydeck-lite list
relaydeck-lite status
```

新增一条 TCP 规则：

```bash
relaydeck-lite add '{"name":"线路A","network":"tcp","listen":"[::]:50001","target":"31.56.37.6:50001"}'
```

修改、删除、重启和停止：

```bash
relaydeck-lite update RULE_ID '{"name":"线路A","network":"both","listen":"[::]:50001","target":"31.56.37.6:50001"}'
relaydeck-lite delete RULE_ID
# restart 重新绑定当前规则；stop 停止转发监听但保留管理 socket
relaydeck-lite restart
relaydeck-lite stop
relaydeck-lite start
```

交互式终端：

```bash
relaydeck-lite shell
```

shell 中支持 `list`、`status`、`add <json>`、`update <id> <json>`、`delete <id>`、`start`、`stop`、`restart`、`shutdown` 和 `help`。
