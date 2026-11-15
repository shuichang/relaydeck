#![cfg_attr(windows, allow(dead_code, unused_imports, unused_variables))]

use serde::{Deserialize, Serialize};
use socket2::{Domain, Protocol, Socket, Type};
use std::{
    collections::HashMap,
    env,
    fs::{self, File},
    io::{self, ErrorKind, Write},
    net::{IpAddr, SocketAddr, ToSocketAddrs},
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc, OnceLock,
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader as AsyncBufReader},
    net::{TcpListener, TcpStream, UdpSocket},
    runtime::Builder,
    sync::{watch, Mutex, Semaphore},
    task::{AbortHandle, JoinHandle, JoinSet},
    time,
};

#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;

#[cfg(unix)]
use std::os::unix::fs::FileTypeExt;

#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};

const DEFAULT_CONFIG: &str = "/etc/relaydeck-lite/rules.json";
const DEFAULT_SOCKET: &str = "/run/relaydeck-lite.sock";
const DEFAULT_MAX_CONNECTIONS: u32 = 512;
const DEFAULT_UDP_TIMEOUT: u64 = 60;
const MAX_RULES: usize = 256;
const GLOBAL_TCP_SESSIONS: usize = 512;
const GLOBAL_UDP_SESSIONS: usize = 256;
const TCP_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const TARGET_RESOLVE_TIMEOUT: Duration = Duration::from_secs(5);
const CONTROL_READ_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_COMMAND_BYTES: usize = 128 * 1024;
const MAX_CONFIG_BYTES: usize = 512 * 1024;
const MAX_RULE_TEXT_BYTES: usize = 4096;

static TARGET_RESOLVER_SLOTS: OnceLock<Arc<Semaphore>> = OnceLock::new();

fn default_enabled() -> bool {
    true
}

fn default_max_connections() -> u32 {
    DEFAULT_MAX_CONNECTIONS
}

fn default_udp_timeout() -> u64 {
    DEFAULT_UDP_TIMEOUT
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct Rule {
    #[serde(default)]
    id: String,
    #[serde(default)]
    name: String,
    #[serde(default = "default_enabled")]
    enabled: bool,
    network: String,
    listen: String,
    target: String,
    #[serde(default = "default_max_connections")]
    max_connections: u32,
    #[serde(default = "default_udp_timeout")]
    udp_timeout: u64,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
struct Config {
    #[serde(default)]
    rules: Vec<Rule>,
}

#[derive(Clone, Debug, Serialize)]
struct RuleStatus {
    id: String,
    name: String,
    enabled: bool,
    network: String,
    listen: String,
    target: String,
    running: bool,
    error: String,
}

#[derive(Default)]
struct Runtime {
    handles: Vec<JoinHandle<()>>,
    cancel: Option<watch::Sender<u64>>,
    generation: u64,
    active: bool,
    error: String,
    statuses: HashMap<String, RuleStatus>,
}

struct Controller {
    config_path: PathBuf,
    config: Config,
    runtime: Runtime,
}

enum CommandResult {
    Text(String),
    Shutdown(String),
}

fn boxed_error(message: impl Into<String>) -> Box<dyn std::error::Error + Send + Sync> {
    message.into().into()
}

async fn abort_handles(handles: &mut Vec<JoinHandle<()>>) {
    for handle in handles.drain(..) {
        handle.abort();
        let _ = handle.await;
    }
}

fn now_id() -> String {
    let micros = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_micros();
    format!("r-{micros:x}")
}

fn epoch_millis() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .min(u128::from(u64::MAX)) as u64
}

fn normalize_listen(raw: &str) -> String {
    let trimmed = raw.trim();
    if trimmed.starts_with(':') {
        format!("[::]{trimmed}")
    } else {
        trimmed.to_string()
    }
}

fn resolve_one(raw: &str) -> Result<SocketAddr, String> {
    let normalized = normalize_listen(raw);
    let mut addresses: Vec<SocketAddr> = normalized
        .to_socket_addrs()
        .map_err(|error| format!("{raw}: {error}"))?
        .collect();
    addresses.sort_by_key(|address| if address.is_ipv4() { 0 } else { 1 });
    addresses
        .into_iter()
        .next()
        .ok_or_else(|| format!("{raw}: no address resolved"))
}

fn resolve_listen(raw: &str) -> Result<SocketAddr, String> {
    let normalized = normalize_listen(raw);
    normalized
        .parse()
        .map_err(|error| format!("{raw}: {error}"))
}

fn endpoint_port(raw: &str) -> Result<u16, String> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return Err("endpoint is empty".to_string());
    }
    if let Ok(address) = normalize_listen(trimmed).parse::<SocketAddr>() {
        return if address.port() == 0 {
            Err("endpoint port must be between 1 and 65535".to_string())
        } else {
            Ok(address.port())
        };
    }

    let port = if trimmed.starts_with('[') {
        let close = trimmed
            .find(']')
            .ok_or_else(|| "IPv6 endpoint is missing ]".to_string())?;
        trimmed
            .get(close + 1..)
            .and_then(|suffix| suffix.strip_prefix(':'))
            .ok_or_else(|| "IPv6 endpoint must use [address]:port".to_string())?
    } else {
        let (host, port) = trimmed
            .rsplit_once(':')
            .ok_or_else(|| "endpoint must use host:port".to_string())?;
        if host.is_empty() || host.contains(':') {
            return Err("IPv6 endpoint must use [address]:port".to_string());
        }
        port
    };
    let port = port
        .parse::<u16>()
        .map_err(|_| "endpoint port must be between 1 and 65535".to_string())?;
    if port == 0 {
        return Err("endpoint port must be between 1 and 65535".to_string());
    }
    Ok(port)
}

fn resolve_target(raw: &str) -> Result<SocketAddr, String> {
    resolve_one(raw)
}

async fn resolve_target_async(raw: &str) -> Result<SocketAddr, String> {
    let raw = raw.to_string();
    let slots = TARGET_RESOLVER_SLOTS
        .get_or_init(|| Arc::new(Semaphore::new(1)))
        .clone();
    let permit = time::timeout(TARGET_RESOLVE_TIMEOUT, slots.acquire_owned())
        .await
        .map_err(|_| format!("target resolver slot timed out after {TARGET_RESOLVE_TIMEOUT:?}"))?
        .map_err(|error| format!("target resolver slot failed: {error}"))?;
    let task = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        resolve_target(&raw)
    });
    time::timeout(TARGET_RESOLVE_TIMEOUT, task)
        .await
        .map_err(|_| format!("target resolution timed out after {TARGET_RESOLVE_TIMEOUT:?}"))?
        .map_err(|error| format!("target resolver task failed: {error}"))?
}

fn is_dual_stack_listen(raw: &str, address: SocketAddr) -> bool {
    address.ip().is_ipv6() && address.ip().is_unspecified() && {
        let trimmed = raw.trim();
        trimmed.starts_with(':') || trimmed.starts_with('[')
    }
}

async fn bind_tcp(raw: &str) -> io::Result<TcpListener> {
    let address = resolve_listen(raw).map_err(io::Error::other)?;
    if !is_dual_stack_listen(raw, address) {
        return TcpListener::bind(address).await;
    }
    let socket = Socket::new(Domain::IPV6, Type::STREAM, Some(Protocol::TCP))?;
    socket.set_only_v6(false)?;
    socket.set_reuse_address(true)?;
    socket.bind(&address.into())?;
    socket.listen(1024)?;
    socket.set_nonblocking(true)?;
    TcpListener::from_std(socket.into())
}

async fn bind_udp(raw: &str) -> io::Result<UdpSocket> {
    let address = resolve_listen(raw).map_err(io::Error::other)?;
    if !is_dual_stack_listen(raw, address) {
        return UdpSocket::bind(address).await;
    }
    let socket = Socket::new(Domain::IPV6, Type::DGRAM, Some(Protocol::UDP))?;
    socket.set_only_v6(false)?;
    socket.set_reuse_address(true)?;
    socket.bind(&address.into())?;
    socket.set_nonblocking(true)?;
    UdpSocket::from_std(socket.into())
}

fn validate_rule(rule: &Rule) -> Result<(), String> {
    if !matches!(rule.network.as_str(), "tcp" | "udp" | "both") {
        return Err("network must be tcp, udp, or both".to_string());
    }
    if rule.listen.trim().is_empty() {
        return Err("listen is empty".to_string());
    }
    if rule.target.trim().is_empty() {
        return Err("target is empty".to_string());
    }
    resolve_listen(&rule.listen)?;
    endpoint_port(&rule.listen)?;
    endpoint_port(&rule.target)?;
    for (field, value) in [
        ("id", rule.id.as_str()),
        ("name", rule.name.as_str()),
        ("listen", rule.listen.as_str()),
        ("target", rule.target.as_str()),
    ] {
        if value.len() > MAX_RULE_TEXT_BYTES {
            return Err(format!("{field} exceeds {MAX_RULE_TEXT_BYTES} bytes"));
        }
    }
    if rule.max_connections == 0 || rule.max_connections > 65_536 {
        return Err("max_connections must be between 1 and 65536".to_string());
    }
    if rule.udp_timeout == 0 || rule.udp_timeout > 86_400 {
        return Err("udp_timeout must be between 1 and 86400 seconds".to_string());
    }
    Ok(())
}

fn validate_config(config: &Config) -> Result<(), String> {
    if config.rules.len() > MAX_RULES {
        return Err(format!("too many rules; maximum is {MAX_RULES}"));
    }
    let mut ids = std::collections::HashSet::with_capacity(config.rules.len());
    for rule in &config.rules {
        if rule.id.trim().is_empty() {
            return Err("rule id is empty".to_string());
        }
        if !ids.insert(rule.id.clone()) {
            return Err(format!("duplicate rule id: {}", rule.id));
        }
        validate_rule(rule)?;
    }
    Ok(())
}

fn load_config(path: &Path) -> Result<Config, Box<dyn std::error::Error + Send + Sync>> {
    if !path.exists() {
        return Ok(Config::default());
    }
    let size = fs::metadata(path)?.len();
    if size > MAX_CONFIG_BYTES as u64 {
        return Err(boxed_error(format!(
            "configuration exceeds {MAX_CONFIG_BYTES} bytes"
        )));
    }
    let data = fs::read(path)?;
    let config: Config = serde_json::from_slice(&data)?;
    validate_config(&config).map_err(boxed_error)?;
    Ok(config)
}

fn save_config(
    path: &Path,
    config: &Config,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    validate_config(config).map_err(boxed_error)?;
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let temp = path.with_extension(format!("json.tmp.{}.{}", std::process::id(), nonce));
    let data = serde_json::to_vec_pretty(config)?;
    if data.len() > MAX_CONFIG_BYTES {
        return Err(boxed_error(format!(
            "configuration exceeds {MAX_CONFIG_BYTES} bytes"
        )));
    }
    let result = (|| -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let mut file = File::create(&temp)?;
        set_private_mode(&temp)?;
        file.write_all(&data)?;
        file.sync_all()?;
        fs::rename(&temp, path)?;
        sync_parent(path)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temp);
    }
    result
}

async fn persist_config(
    path: &Path,
    config: &Config,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let path = path.to_path_buf();
    let config = config.clone();
    tokio::task::spawn_blocking(move || save_config(&path, &config))
        .await
        .map_err(|error| boxed_error(format!("configuration writer task failed: {error}")))?
}

fn set_private_mode(path: &Path) -> io::Result<()> {
    #[cfg(unix)]
    {
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

fn sync_parent(path: &Path) -> io::Result<()> {
    #[cfg(unix)]
    if let Some(parent) = path.parent() {
        File::open(parent)?.sync_all()?;
    }
    Ok(())
}

impl Controller {
    async fn start(&mut self) -> Result<(), String> {
        if self.runtime.active {
            return Ok(());
        }
        self.runtime.generation = self.runtime.generation.wrapping_add(1);
        let (cancel, receiver) = watch::channel(self.runtime.generation);
        let mut handles = Vec::new();
        let mut statuses = HashMap::new();
        let global_tcp = Arc::new(Semaphore::new(GLOBAL_TCP_SESSIONS));
        let global_udp = Arc::new(Semaphore::new(GLOBAL_UDP_SESSIONS));

        for rule in self.config.rules.iter().filter(|rule| rule.enabled) {
            let status = RuleStatus {
                id: rule.id.clone(),
                name: rule.name.clone(),
                enabled: rule.enabled,
                network: rule.network.clone(),
                listen: rule.listen.clone(),
                target: rule.target.clone(),
                running: false,
                error: String::new(),
            };
            statuses.insert(rule.id.clone(), status);

            let target = match resolve_target_async(&rule.target).await {
                Ok(target) => target,
                Err(error) => {
                    abort_handles(&mut handles).await;
                    let message = format!("rule {} target: {error}", rule.id);
                    statuses.values_mut().for_each(|status| {
                        status.error = message.clone();
                    });
                    self.runtime.error = message.clone();
                    self.runtime.statuses = statuses;
                    return Err(message);
                }
            };
            let listen = match resolve_listen(&rule.listen) {
                Ok(listen) => listen,
                Err(error) => {
                    abort_handles(&mut handles).await;
                    let message = format!("rule {} listen: {error}", rule.id);
                    statuses.values_mut().for_each(|status| {
                        status.error = message.clone();
                    });
                    self.runtime.error = message.clone();
                    self.runtime.statuses = statuses;
                    return Err(message);
                }
            };

            let tcp_listener = if rule.network == "tcp" || rule.network == "both" {
                match bind_tcp(&rule.listen).await {
                    Ok(listener) => Some(listener),
                    Err(error) => {
                        abort_handles(&mut handles).await;
                        let message = format!("rule {} TCP bind {}: {error}", rule.id, listen);
                        statuses.values_mut().for_each(|status| {
                            status.error = message.clone();
                        });
                        self.runtime.error = message.clone();
                        self.runtime.statuses = statuses;
                        return Err(message);
                    }
                }
            } else {
                None
            };
            let udp_socket = if rule.network == "udp" || rule.network == "both" {
                match bind_udp(&rule.listen).await {
                    Ok(socket) => Some(socket),
                    Err(error) => {
                        abort_handles(&mut handles).await;
                        let message = format!("rule {} UDP bind {}: {error}", rule.id, listen);
                        statuses.values_mut().for_each(|status| {
                            status.error = message.clone();
                        });
                        self.runtime.error = message.clone();
                        self.runtime.statuses = statuses;
                        return Err(message);
                    }
                }
            } else {
                None
            };
            let rule_semaphore = Arc::new(Semaphore::new(rule.max_connections as usize));
            if let Some(listener) = tcp_listener {
                handles.push(tokio::spawn(run_tcp(
                    listener,
                    target,
                    rule_semaphore.clone(),
                    global_tcp.clone(),
                    receiver.clone(),
                )));
            }
            if let Some(socket) = udp_socket {
                handles.push(tokio::spawn(run_udp(
                    rule.clone(),
                    socket,
                    target,
                    rule_semaphore,
                    global_udp.clone(),
                    receiver.clone(),
                )));
            }
        }

        self.runtime.cancel = Some(cancel);
        self.runtime.handles = handles;
        self.runtime.error.clear();
        self.runtime.statuses = statuses
            .into_iter()
            .map(|(id, mut status)| {
                status.running = true;
                (id, status)
            })
            .collect();
        self.runtime.active = true;
        Ok(())
    }

    async fn stop(&mut self) {
        self.runtime.generation = self.runtime.generation.wrapping_add(1);
        if let Some(cancel) = self.runtime.cancel.take() {
            let _ = cancel.send(self.runtime.generation);
        }
        for handle in self.runtime.handles.drain(..) {
            let _ = handle.await;
        }
        for status in self.runtime.statuses.values_mut() {
            status.running = false;
        }
        self.runtime.active = false;
    }

    async fn restart(&mut self) -> Result<(), String> {
        self.stop().await;
        self.start().await
    }

    fn status_json(&self) -> Result<String, String> {
        let statuses: Vec<&RuleStatus> = self.runtime.statuses.values().collect();
        serde_json::to_string_pretty(&serde_json::json!({
            "active": self.runtime.active,
            "error": self.runtime.error,
            "rules": statuses,
        }))
        .map_err(|error| error.to_string())
    }

    async fn commit_config(&mut self, candidate: Config) -> Result<(), String> {
        validate_config(&candidate)?;
        let previous = self.config.clone();
        if let Err(error) = persist_config(&self.config_path, &candidate).await {
            let restore_error = persist_config(&self.config_path, &previous)
                .await
                .err()
                .map(|restore| restore.to_string());
            return Err(match restore_error {
                None => format!("configuration save failed and was rolled back: {error}"),
                Some(restore) => {
                    format!("configuration save failed: {error}; rollback failed: {restore}")
                }
            });
        }
        self.config = candidate;
        if !self.runtime.active {
            return Ok(());
        }
        if let Err(error) = self.restart().await {
            self.config = previous.clone();
            let restore_error = persist_config(&self.config_path, &previous)
                .await
                .err()
                .map(|restore| restore.to_string());
            let restart_error = self.restart().await.err();
            return Err(match (restore_error, restart_error) {
                (None, None) => format!("new configuration failed and was rolled back: {error}"),
                (restore, restart) => format!(
                    "new configuration failed: {error}; rollback failed: restore={restore:?}, restart={restart:?}"
                ),
            });
        }
        Ok(())
    }
}

async fn run_tcp(
    listener: TcpListener,
    target: SocketAddr,
    rule_semaphore: Arc<Semaphore>,
    global_semaphore: Arc<Semaphore>,
    mut cancel: watch::Receiver<u64>,
) {
    let mut connections = JoinSet::new();
    loop {
        tokio::select! {
            changed = cancel.changed() => {
                if changed.is_ok() {
                    break;
                }
            }
            accepted = listener.accept() => {
                match accepted {
                    Ok((mut client, _peer)) => {
                        let Ok(global_permit) = global_semaphore.clone().try_acquire_owned() else {
                            let _ = client.shutdown().await;
                            continue;
                        };
                        let Ok(rule_permit) = rule_semaphore.clone().try_acquire_owned() else {
                            let _ = client.shutdown().await;
                            continue;
                        };
                        connections.spawn(async move {
                            let _global_permit = global_permit;
                            let _rule_permit = rule_permit;
                            let Ok(Ok(mut upstream)) =
                                time::timeout(TCP_CONNECT_TIMEOUT, TcpStream::connect(target)).await
                            else {
                                let _ = client.shutdown().await;
                                return;
                            };
                            let _ = client.set_nodelay(true);
                            let _ = upstream.set_nodelay(true);
                            let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
                            let _ = client.shutdown().await;
                            let _ = upstream.shutdown().await;
                        });
                    }
                    Err(_) => {
                        time::sleep(Duration::from_millis(25)).await;
                    }
                }
            }
        }
        while connections.try_join_next().is_some() {}
    }
    connections.abort_all();
    while connections.join_next().await.is_some() {}
}

struct UdpAssociation {
    socket: Arc<UdpSocket>,
    last_seen: Arc<AtomicU64>,
    abort: AbortHandle,
    _permit: tokio::sync::OwnedSemaphorePermit,
    _rule_permit: tokio::sync::OwnedSemaphorePermit,
}

async fn bind_udp_target(target: SocketAddr) -> io::Result<UdpSocket> {
    let local = match target.ip() {
        IpAddr::V4(_) => "0.0.0.0:0",
        IpAddr::V6(_) => "[::]:0",
    };
    let socket = UdpSocket::bind(local).await?;
    socket.connect(target).await?;
    Ok(socket)
}

async fn run_udp(
    rule: Rule,
    socket: UdpSocket,
    target: SocketAddr,
    rule_semaphore: Arc<Semaphore>,
    global_semaphore: Arc<Semaphore>,
    mut cancel: watch::Receiver<u64>,
) {
    let socket = Arc::new(socket);
    let mut associations: HashMap<SocketAddr, UdpAssociation> = HashMap::new();
    let mut workers = JoinSet::new();
    let (dead_tx, mut dead_rx) = tokio::sync::mpsc::unbounded_channel();
    let mut buffer = vec![0_u8; 65_536];
    let mut cleanup = time::interval(Duration::from_secs(5));
    cleanup.set_missed_tick_behavior(time::MissedTickBehavior::Delay);

    loop {
        tokio::select! {
            changed = cancel.changed() => {
                if changed.is_ok() {
                    break;
                }
            }
            dead = dead_rx.recv() => {
                if let Some(peer) = dead {
                    if let Some(association) = associations.remove(&peer) {
                        association.abort.abort();
                    }
                }
            }
            received = socket.recv_from(&mut buffer) => {
                let Ok((size, peer)) = received else {
                    time::sleep(Duration::from_millis(25)).await;
                    continue;
                };
                let association = if let Some(association) = associations.get_mut(&peer) {
                    association.last_seen.store(epoch_millis(), Ordering::Relaxed);
                    Some(association.socket.clone())
                } else if associations.len() < rule.max_connections as usize {
                    let Ok(permit) = global_semaphore.clone().try_acquire_owned() else {
                        continue;
                    };
                    let Ok(rule_permit) = rule_semaphore.clone().try_acquire_owned() else {
                        drop(permit);
                        continue;
                    };
                    let Ok(target_socket) = bind_udp_target(target).await else {
                        drop(permit);
                        drop(rule_permit);
                        continue;
                    };
                    let target_socket = Arc::new(target_socket);
                    let reply_socket = socket.clone();
                    let reply_target = target_socket.clone();
                    let last_seen = Arc::new(AtomicU64::new(epoch_millis()));
                    let worker_last_seen = last_seen.clone();
                    let worker_dead = dead_tx.clone();
                    let abort = workers.spawn(async move {
                        let mut response = vec![0_u8; 65_536];
                        loop {
                            let Ok(size) = reply_target.recv(&mut response).await else {
                                break;
                            };
                            worker_last_seen.store(epoch_millis(), Ordering::Relaxed);
                            if reply_socket.send_to(&response[..size], peer).await.is_err() {
                                break;
                            }
                        }
                        let _ = worker_dead.send(peer);
                    });
                    associations.insert(peer, UdpAssociation {
                        socket: target_socket.clone(),
                        last_seen,
                        abort,
                        _permit: permit,
                        _rule_permit: rule_permit,
                    });
                    Some(target_socket)
                } else {
                    None
                };
                if let Some(target_socket) = association {
                    let _ = target_socket.send(&buffer[..size]).await;
                }
            }
            _ = cleanup.tick() => {
                let timeout = rule.udp_timeout.saturating_mul(1000);
                let now = epoch_millis();
                associations.retain(|_, association| {
                    if now.saturating_sub(association.last_seen.load(Ordering::Relaxed)) > timeout {
                        association.abort.abort();
                        false
                    } else {
                        true
                    }
                });
                while workers.try_join_next().is_some() {}
            }
        }
    }
    for association in associations.values() {
        association.abort.abort();
    }
    workers.abort_all();
    while workers.join_next().await.is_some() {}
}

async fn handle_command(
    line: &str,
    controller: &Arc<Mutex<Controller>>,
) -> Result<CommandResult, String> {
    let command = line.trim();
    if command.is_empty() {
        return Ok(CommandResult::Text(String::new()));
    }
    if command == "help" {
        return Ok(CommandResult::Text(
            "list | status | add <json> | update <id> <json> | delete <id> | start | stop | restart | shutdown"
                .to_string(),
        ));
    }

    let (name, argument) = command
        .split_once(' ')
        .map_or((command, ""), |(name, argument)| (name, argument.trim()));
    let mut guard = controller.lock().await;
    match name {
        "list" => serde_json::to_string_pretty(&guard.config.rules)
            .map(CommandResult::Text)
            .map_err(|error| error.to_string()),
        "status" => guard.status_json().map(CommandResult::Text),
        "start" => {
            guard.start().await?;
            Ok(CommandResult::Text("ok started".to_string()))
        }
        "stop" => {
            guard.stop().await;
            Ok(CommandResult::Text("ok stopped".to_string()))
        }
        "restart" => {
            guard.restart().await?;
            Ok(CommandResult::Text("ok restarted".to_string()))
        }
        "shutdown" => {
            guard.stop().await;
            Ok(CommandResult::Shutdown("ok shutting down".to_string()))
        }
        "add" => {
            let mut rule: Rule =
                serde_json::from_str(argument).map_err(|error| error.to_string())?;
            if rule.id.is_empty() {
                rule.id = now_id();
            }
            validate_rule(&rule)?;
            if guard
                .config
                .rules
                .iter()
                .any(|existing| existing.id == rule.id)
            {
                return Err(format!("rule id already exists: {}", rule.id));
            }
            let mut candidate = guard.config.clone();
            candidate.rules.push(rule);
            guard.commit_config(candidate).await?;
            Ok(CommandResult::Text("ok added".to_string()))
        }
        "update" => {
            let (id, json) = argument
                .split_once(' ')
                .ok_or_else(|| "usage: update <id> <json>".to_string())?;
            let mut rule: Rule =
                serde_json::from_str(json.trim()).map_err(|error| error.to_string())?;
            rule.id = id.to_string();
            validate_rule(&rule)?;
            let mut candidate = guard.config.clone();
            let existing = candidate
                .rules
                .iter_mut()
                .find(|existing| existing.id == id)
                .ok_or_else(|| format!("rule not found: {id}"))?;
            *existing = rule;
            guard.commit_config(candidate).await?;
            Ok(CommandResult::Text("ok updated".to_string()))
        }
        "delete" => {
            let mut candidate = guard.config.clone();
            let old_len = candidate.rules.len();
            candidate.rules.retain(|rule| rule.id != argument);
            if candidate.rules.len() == old_len {
                return Err(format!("rule not found: {argument}"));
            }
            guard.commit_config(candidate).await?;
            Ok(CommandResult::Text("ok deleted".to_string()))
        }
        _ => Err(format!("unknown command: {name}; use help")),
    }
}

#[cfg(unix)]
async fn read_control_line(stream: &mut UnixStream) -> Result<Option<String>, String> {
    let mut data = Vec::with_capacity(256);
    let mut chunk = [0_u8; 1024];
    loop {
        let count = time::timeout(CONTROL_READ_TIMEOUT, stream.read(&mut chunk))
            .await
            .map_err(|_| "control command read timed out".to_string())?
            .map_err(|error| error.to_string())?;
        if count == 0 {
            return if data.is_empty() {
                Ok(None)
            } else {
                String::from_utf8(data)
                    .map(Some)
                    .map_err(|_| "control command must be UTF-8".to_string())
            };
        }
        let end = chunk[..count]
            .iter()
            .position(|byte| *byte == b'\n')
            .map_or(count, |position| position + 1);
        if data.len() + end > MAX_COMMAND_BYTES {
            return Err(format!("control command exceeds {MAX_COMMAND_BYTES} bytes"));
        }
        data.extend_from_slice(&chunk[..end]);
        if end < count || data.ends_with(b"\n") {
            break;
        }
    }
    String::from_utf8(data)
        .map(Some)
        .map_err(|_| "control command must be UTF-8".to_string())
}

#[cfg(unix)]
async fn handle_control_connection(
    mut stream: UnixStream,
    controller: Arc<Mutex<Controller>>,
    shutdown: watch::Sender<bool>,
) {
    let line = match read_control_line(&mut stream).await {
        Ok(Some(line)) => line,
        Ok(None) => return,
        Err(error) => {
            let _ = stream
                .write_all(format!("error: {error}\n").as_bytes())
                .await;
            return;
        }
    };
    let result = handle_command(&line, &controller).await;
    let should_shutdown = matches!(&result, Ok(CommandResult::Shutdown(_)));
    let response = match result {
        Ok(CommandResult::Text(text)) | Ok(CommandResult::Shutdown(text)) => text,
        Err(error) => format!("error: {error}"),
    };
    if stream
        .write_all(format!("{response}\n").as_bytes())
        .await
        .is_err()
    {
        return;
    }
    let _ = stream.shutdown().await;
    if should_shutdown {
        let _ = shutdown.send(true);
    }
}

#[cfg(unix)]
async fn bind_control_listener(
    socket_path: &Path,
) -> Result<UnixListener, Box<dyn std::error::Error + Send + Sync>> {
    match fs::symlink_metadata(socket_path) {
        Ok(metadata) => {
            if !metadata.file_type().is_socket() {
                return Err(boxed_error(format!(
                    "refusing to remove non-socket control path: {}",
                    socket_path.display()
                )));
            }
            match UnixStream::connect(socket_path).await {
                Ok(_) => {
                    return Err(boxed_error(format!(
                        "control socket is already in use: {}",
                        socket_path.display()
                    )));
                }
                Err(error)
                    if matches!(
                        error.kind(),
                        ErrorKind::ConnectionRefused | ErrorKind::NotFound
                    ) => {}
                Err(error) => {
                    return Err(boxed_error(format!(
                        "cannot inspect existing control socket {}: {error}",
                        socket_path.display()
                    )));
                }
            }
            fs::remove_file(socket_path)?;
        }
        Err(error) if error.kind() == ErrorKind::NotFound => {}
        Err(error) => return Err(error.into()),
    }
    if let Some(parent) = socket_path.parent() {
        fs::create_dir_all(parent)?;
    }
    let listener = UnixListener::bind(socket_path)?;
    fs::set_permissions(socket_path, fs::Permissions::from_mode(0o600))?;
    Ok(listener)
}

#[cfg(unix)]
async fn control_server(
    listener: UnixListener,
    controller: Arc<Mutex<Controller>>,
    shutdown: watch::Sender<bool>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    loop {
        let (stream, _) = match listener.accept().await {
            Ok(connection) => connection,
            Err(_) => {
                time::sleep(Duration::from_millis(25)).await;
                continue;
            }
        };
        tokio::spawn(handle_control_connection(
            stream,
            controller.clone(),
            shutdown.clone(),
        ));
    }
}

#[cfg(unix)]
async fn send_command(
    socket_path: &Path,
    command: &str,
) -> Result<String, Box<dyn std::error::Error + Send + Sync>> {
    let mut stream = UnixStream::connect(socket_path).await?;
    stream.write_all(command.as_bytes()).await?;
    stream.write_all(b"\n").await?;
    stream.shutdown().await?;
    let mut response = String::new();
    let mut reader = AsyncBufReader::new(stream);
    reader.read_to_string(&mut response).await?;
    Ok(response)
}

#[cfg(not(unix))]
async fn send_command(
    _socket_path: &Path,
    _command: &str,
) -> Result<String, Box<dyn std::error::Error + Send + Sync>> {
    Err(boxed_error(
        "RelayDeck Lite control sockets are supported on Linux and other Unix systems only",
    ))
}

async fn shell(socket_path: PathBuf) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let stdin = tokio::io::stdin();
    let mut lines = AsyncBufReader::new(stdin).lines();
    let mut stdout = tokio::io::stdout();
    loop {
        stdout.write_all(b"relay> ").await?;
        stdout.flush().await?;
        let Some(line) = lines.next_line().await? else {
            break;
        };
        let command = line.trim();
        if command == "exit" || command == "quit" {
            break;
        }
        let response = send_command(&socket_path, command).await?;
        stdout.write_all(response.as_bytes()).await?;
    }
    Ok(())
}

fn option(args: &[String], name: &str, default: &str) -> String {
    args.windows(2)
        .find(|pair| pair[0] == name)
        .map(|pair| pair[1].clone())
        .unwrap_or_else(|| default.to_string())
}

#[cfg(unix)]
async fn run_daemon(
    config_path: PathBuf,
    socket_path: PathBuf,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let listener = bind_control_listener(&socket_path).await?;
    let (config, config_error) = match load_config(&config_path) {
        Ok(config) => (config, None),
        Err(error) => (Config::default(), Some(error.to_string())),
    };
    let controller = Arc::new(Mutex::new(Controller {
        config_path,
        config,
        runtime: Runtime::default(),
    }));
    let (shutdown_tx, mut shutdown_rx) = watch::channel(false);
    let socket_for_cleanup = socket_path.clone();
    let mut control = tokio::spawn(control_server(listener, controller.clone(), shutdown_tx));
    if let Some(error) = config_error {
        let mut guard = controller.lock().await;
        guard.runtime.error = format!("configuration load failed: {error}");
        eprintln!("relaydeck-lite: {}", guard.runtime.error);
    } else if let Err(error) = controller.lock().await.start().await {
        eprintln!("relaydeck-lite: initial rule start failed: {error}");
    }
    tokio::select! {
        result = &mut control => {
            result??;
        }
        _ = shutdown_rx.changed() => {}
        _ = tokio::signal::ctrl_c() => {}
        _ = terminate_signal() => {}
    }
    control.abort();
    controller.lock().await.stop().await;
    let _ = fs::remove_file(socket_for_cleanup);
    Ok(())
}

#[cfg(not(unix))]
async fn run_daemon(
    _config_path: PathBuf,
    _socket_path: PathBuf,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    Err(boxed_error(
        "RelayDeck Lite daemon is supported on Linux and other Unix systems only",
    ))
}

#[cfg(unix)]
async fn terminate_signal() -> io::Result<()> {
    use tokio::signal::unix::{signal, SignalKind};
    let mut signal = signal(SignalKind::terminate())?;
    signal.recv().await;
    Ok(())
}

#[cfg(not(unix))]
async fn terminate_signal() -> io::Result<()> {
    std::future::pending::<()>().await;
    Ok(())
}

fn usage() {
    println!(
        "RelayDeck Lite\n\
         Usage:\n\
           relaydeck-lite run [--config PATH] [--socket PATH]\n\
           relaydeck-lite shell [--socket PATH]\n\
           relaydeck-lite list|status|start|stop|restart|shutdown [--socket PATH]\n\
           relaydeck-lite add <JSON> [--socket PATH]\n\
           relaydeck-lite update <ID> <JSON> [--socket PATH]\n\
           relaydeck-lite delete <ID> [--socket PATH]"
    );
}

fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let args: Vec<String> = env::args().collect();
    let command = args.get(1).map(String::as_str).unwrap_or("help");
    let runtime = Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|error| boxed_error(error.to_string()))?;

    runtime.block_on(async {
        match command {
            "run" => {
                let config = PathBuf::from(option(&args, "--config", DEFAULT_CONFIG));
                let socket = PathBuf::from(option(&args, "--socket", DEFAULT_SOCKET));
                run_daemon(config, socket).await
            }
            "shell" => {
                let socket = PathBuf::from(option(&args, "--socket", DEFAULT_SOCKET));
                shell(socket).await
            }
            "help" | "--help" | "-h" => {
                usage();
                Ok(())
            }
            command @ ("list" | "status" | "start" | "stop" | "restart" | "shutdown") => {
                let socket = PathBuf::from(option(&args, "--socket", DEFAULT_SOCKET));
                print!("{}", send_command(&socket, command).await?);
                Ok(())
            }
            "add" => {
                let json = args
                    .get(2)
                    .ok_or_else(|| boxed_error("add requires JSON"))?;
                let socket = PathBuf::from(option(&args, "--socket", DEFAULT_SOCKET));
                print!("{}", send_command(&socket, &format!("add {json}")).await?);
                Ok(())
            }
            "update" => {
                let id = args
                    .get(2)
                    .ok_or_else(|| boxed_error("update requires ID"))?;
                let json = args
                    .get(3)
                    .ok_or_else(|| boxed_error("update requires JSON"))?;
                let socket = PathBuf::from(option(&args, "--socket", DEFAULT_SOCKET));
                print!(
                    "{}",
                    send_command(&socket, &format!("update {id} {json}")).await?
                );
                Ok(())
            }
            "delete" => {
                let id = args
                    .get(2)
                    .ok_or_else(|| boxed_error("delete requires ID"))?;
                let socket = PathBuf::from(option(&args, "--socket", DEFAULT_SOCKET));
                print!("{}", send_command(&socket, &format!("delete {id}")).await?);
                Ok(())
            }
            _ => {
                usage();
                Err(boxed_error(format!("unknown command: {command}")))
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalizes_ipv6_wildcard_short_form() {
        assert_eq!(normalize_listen(":50001"), "[::]:50001");
        assert_eq!(normalize_listen("[::]:50001"), "[::]:50001");
        assert_eq!(normalize_listen("0.0.0.0:50001"), "0.0.0.0:50001");
    }

    #[test]
    fn rule_defaults_and_validation_are_stable() {
        let rule: Rule =
            serde_json::from_str(r#"{"network":"tcp","listen":":50001","target":"127.0.0.1:1"}"#)
                .expect("rule JSON");
        assert!(rule.enabled);
        assert_eq!(rule.max_connections, DEFAULT_MAX_CONNECTIONS);
        assert_eq!(rule.udp_timeout, DEFAULT_UDP_TIMEOUT);
        assert!(validate_rule(&rule).is_ok());
    }

    #[test]
    fn invalid_network_is_rejected() {
        let rule = Rule {
            id: "test".to_string(),
            name: String::new(),
            enabled: true,
            network: "icmp".to_string(),
            listen: ":50001".to_string(),
            target: "127.0.0.1:1".to_string(),
            max_connections: DEFAULT_MAX_CONNECTIONS,
            udp_timeout: DEFAULT_UDP_TIMEOUT,
        };
        assert!(validate_rule(&rule).is_err());
    }

    #[test]
    fn hostname_target_is_validated_without_dns_lookup() {
        let rule = Rule {
            id: "test".to_string(),
            name: "test".to_string(),
            enabled: true,
            network: "tcp".to_string(),
            listen: ":50001".to_string(),
            target: "not-resolvable.invalid:443".to_string(),
            max_connections: DEFAULT_MAX_CONNECTIONS,
            udp_timeout: DEFAULT_UDP_TIMEOUT,
        };
        assert!(validate_rule(&rule).is_ok());
    }

    #[test]
    fn hostname_listen_is_rejected() {
        let rule = Rule {
            id: "test".to_string(),
            name: "test".to_string(),
            enabled: true,
            network: "tcp".to_string(),
            listen: "localhost:50001".to_string(),
            target: "127.0.0.1:443".to_string(),
            max_connections: DEFAULT_MAX_CONNECTIONS,
            udp_timeout: DEFAULT_UDP_TIMEOUT,
        };
        assert!(validate_rule(&rule).is_err());
    }

    #[test]
    fn endpoint_ports_require_bracketed_ipv6() {
        assert_eq!(endpoint_port("example.com:443").expect("domain port"), 443);
        assert_eq!(endpoint_port("[2001:db8::1]:443").expect("IPv6 port"), 443);
        assert!(endpoint_port("2001:db8::1:443").is_err());
    }

    fn test_rule(network: &str, timeout: u64) -> Rule {
        Rule {
            id: "test".to_string(),
            name: "test".to_string(),
            enabled: true,
            network: network.to_string(),
            listen: "127.0.0.1:0".to_string(),
            target: "127.0.0.1:0".to_string(),
            max_connections: 8,
            udp_timeout: timeout,
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn tcp_copy_round_trip() {
        let target = TcpListener::bind("127.0.0.1:0").await.expect("target bind");
        let target_addr = target.local_addr().expect("target address");
        let target_task = tokio::spawn(async move {
            let (mut stream, _) = target.accept().await.expect("target accept");
            let mut data = [0_u8; 4];
            stream.read_exact(&mut data).await.expect("target read");
            stream.write_all(&data).await.expect("target write");
        });

        let listener = TcpListener::bind("127.0.0.1:0").await.expect("listen bind");
        let listen_addr = listener.local_addr().expect("listen address");
        let (cancel, receiver) = watch::channel(0);
        let handle = tokio::spawn(run_tcp(
            listener,
            target_addr,
            Arc::new(Semaphore::new(8)),
            Arc::new(Semaphore::new(GLOBAL_TCP_SESSIONS)),
            receiver,
        ));

        let mut client = TcpStream::connect(listen_addr)
            .await
            .expect("client connect");
        client.write_all(b"test").await.expect("client write");
        let mut data = [0_u8; 4];
        client.read_exact(&mut data).await.expect("client read");
        assert_eq!(&data, b"test");
        let _ = cancel.send(1);
        handle.await.expect("relay task");
        target_task.await.expect("target task");
    }

    #[tokio::test(flavor = "current_thread")]
    async fn udp_copy_round_trip() {
        let target = UdpSocket::bind("127.0.0.1:0").await.expect("target bind");
        let target_addr = target.local_addr().expect("target address");
        let target_task = tokio::spawn(async move {
            let mut data = [0_u8; 4];
            let (size, peer) = target.recv_from(&mut data).await.expect("target receive");
            target
                .send_to(&data[..size], peer)
                .await
                .expect("target send");
        });

        let listener = UdpSocket::bind("127.0.0.1:0").await.expect("listen bind");
        let listen_addr = listener.local_addr().expect("listen address");
        let (cancel, receiver) = watch::channel(0);
        let handle = tokio::spawn(run_udp(
            test_rule("udp", 5),
            listener,
            target_addr,
            Arc::new(Semaphore::new(8)),
            Arc::new(Semaphore::new(GLOBAL_UDP_SESSIONS)),
            receiver,
        ));

        let client = UdpSocket::bind("127.0.0.1:0").await.expect("client bind");
        client
            .send_to(b"test", listen_addr)
            .await
            .expect("client send");
        let mut data = [0_u8; 4];
        let (size, _) = time::timeout(Duration::from_secs(2), client.recv_from(&mut data))
            .await
            .expect("client timeout")
            .expect("client receive");
        assert_eq!(&data[..size], b"test");
        let _ = cancel.send(1);
        handle.await.expect("relay task");
        target_task.await.expect("target task");
    }
}
