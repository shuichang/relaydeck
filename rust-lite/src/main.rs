use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    env,
    fs::{self, File},
    io::{self, Write},
    net::{IpAddr, SocketAddr, ToSocketAddrs},
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader as AsyncBufReader},
    net::{TcpListener, TcpStream, UdpSocket},
    runtime::Builder,
    sync::{watch, Mutex, Semaphore},
    task::{AbortHandle, JoinHandle, JoinSet},
    time::{self, Instant},
};

#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;

#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};

const DEFAULT_CONFIG: &str = "/etc/relaydeck-lite/rules.json";
const DEFAULT_SOCKET: &str = "/run/relaydeck-lite.sock";
const DEFAULT_MAX_CONNECTIONS: u32 = 512;
const DEFAULT_UDP_TIMEOUT: u64 = 60;
const MAX_RULES: usize = 256;

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
    normalized
        .to_socket_addrs()
        .map_err(|error| format!("{raw}: {error}"))?
        .next()
        .ok_or_else(|| format!("{raw}: no address resolved"))
}

fn resolve_target(raw: &str) -> Result<SocketAddr, String> {
    resolve_one(raw)
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
    let _ = resolve_one(&rule.listen)?;
    let _ = resolve_target(&rule.target)?;
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
    let temp = path.with_extension("json.tmp");
    let data = serde_json::to_vec_pretty(config)?;
    {
        let mut file = File::create(&temp)?;
        file.write_all(&data)?;
        file.sync_all()?;
    }
    fs::rename(temp, path)?;
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

            let target = match resolve_target(&rule.target) {
                Ok(target) => target,
                Err(error) => {
                    abort_handles(&mut handles).await;
                    self.runtime.statuses = statuses;
                    return Err(format!("rule {} target: {error}", rule.id));
                }
            };
            let listen = match resolve_one(&rule.listen) {
                Ok(listen) => listen,
                Err(error) => {
                    abort_handles(&mut handles).await;
                    self.runtime.statuses = statuses;
                    return Err(format!("rule {} listen: {error}", rule.id));
                }
            };

            let tcp_listener = if rule.network == "tcp" || rule.network == "both" {
                match TcpListener::bind(listen).await {
                    Ok(listener) => Some(listener),
                    Err(error) => {
                        abort_handles(&mut handles).await;
                        return Err(format!("rule {} TCP bind {}: {error}", rule.id, listen));
                    }
                }
            } else {
                None
            };
            let udp_socket = if rule.network == "udp" || rule.network == "both" {
                match UdpSocket::bind(listen).await {
                    Ok(socket) => Some(socket),
                    Err(error) => {
                        abort_handles(&mut handles).await;
                        return Err(format!("rule {} UDP bind {}: {error}", rule.id, listen));
                    }
                }
            } else {
                None
            };
            if let Some(listener) = tcp_listener {
                let semaphore = Arc::new(Semaphore::new(rule.max_connections as usize));
                handles.push(tokio::spawn(run_tcp(
                    rule.clone(),
                    listener,
                    target,
                    semaphore,
                    receiver.clone(),
                )));
            }
            if let Some(socket) = udp_socket {
                handles.push(tokio::spawn(run_udp(
                    rule.clone(),
                    socket,
                    target,
                    receiver.clone(),
                )));
            }
        }

        self.runtime.cancel = Some(cancel);
        self.runtime.handles = handles;
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
            "rules": statuses,
        }))
        .map_err(|error| error.to_string())
    }
}

async fn run_tcp(
    rule: Rule,
    listener: TcpListener,
    target: SocketAddr,
    semaphore: Arc<Semaphore>,
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
                let Ok((mut client, _peer)) = accepted else {
                    continue;
                };
                let Ok(permit) = semaphore.clone().try_acquire_owned() else {
                    let _ = client.shutdown().await;
                    continue;
                };
                let rule = rule.clone();
                connections.spawn(async move {
                    let _permit = permit;
                    let Ok(mut upstream) = TcpStream::connect(target).await else {
                        let _ = client.shutdown().await;
                        return;
                    };
                    let _ = client.set_nodelay(true);
                    let _ = upstream.set_nodelay(true);
                    let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
                    let _ = client.shutdown().await;
                    let _ = upstream.shutdown().await;
                    let _ = rule;
                });
            }
        }
        while connections.try_join_next().is_some() {}
    }
    connections.abort_all();
    while connections.join_next().await.is_some() {}
}

struct UdpAssociation {
    socket: Arc<UdpSocket>,
    last_seen: Instant,
    abort: AbortHandle,
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
    mut cancel: watch::Receiver<u64>,
) {
    let socket = Arc::new(socket);
    let mut associations: HashMap<SocketAddr, UdpAssociation> = HashMap::new();
    let mut workers = JoinSet::new();
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
            received = socket.recv_from(&mut buffer) => {
                let Ok((size, peer)) = received else {
                    continue;
                };
                let association = if let Some(association) = associations.get_mut(&peer) {
                    association.last_seen = Instant::now();
                    Some(association.socket.clone())
                } else if associations.len() < rule.max_connections as usize {
                    let Ok(target_socket) = bind_udp_target(target).await else {
                        continue;
                    };
                    let target_socket = Arc::new(target_socket);
                    let reply_socket = socket.clone();
                    let reply_target = target_socket.clone();
                    let abort = workers.spawn(async move {
                        let mut response = vec![0_u8; 65_536];
                        loop {
                            let Ok(size) = reply_target.recv(&mut response).await else {
                                break;
                            };
                            if reply_socket.send_to(&response[..size], peer).await.is_err() {
                                break;
                            }
                        }
                    });
                    associations.insert(peer, UdpAssociation {
                        socket: target_socket.clone(),
                        last_seen: Instant::now(),
                        abort,
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
                let timeout = Duration::from_secs(rule.udp_timeout);
                associations.retain(|_, association| {
                    if association.last_seen.elapsed() > timeout {
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
            guard.config.rules.push(rule);
            save_config(&guard.config_path, &guard.config).map_err(|error| error.to_string())?;
            if guard.runtime.active {
                guard.restart().await?;
            }
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
            let existing = guard
                .config
                .rules
                .iter_mut()
                .find(|existing| existing.id == id)
                .ok_or_else(|| format!("rule not found: {id}"))?;
            *existing = rule;
            save_config(&guard.config_path, &guard.config).map_err(|error| error.to_string())?;
            if guard.runtime.active {
                guard.restart().await?;
            }
            Ok(CommandResult::Text("ok updated".to_string()))
        }
        "delete" => {
            let old_len = guard.config.rules.len();
            guard.config.rules.retain(|rule| rule.id != argument);
            if guard.config.rules.len() == old_len {
                return Err(format!("rule not found: {argument}"));
            }
            save_config(&guard.config_path, &guard.config).map_err(|error| error.to_string())?;
            if guard.runtime.active {
                guard.restart().await?;
            }
            Ok(CommandResult::Text("ok deleted".to_string()))
        }
        _ => Err(format!("unknown command: {name}; use help")),
    }
}

#[cfg(unix)]
async fn control_server(
    socket_path: PathBuf,
    controller: Arc<Mutex<Controller>>,
    shutdown: tokio::sync::oneshot::Sender<()>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    if socket_path.exists() {
        fs::remove_file(&socket_path)?;
    }
    if let Some(parent) = socket_path.parent() {
        fs::create_dir_all(parent)?;
    }
    let listener = UnixListener::bind(&socket_path)?;
    let _ = fs::set_permissions(&socket_path, fs::Permissions::from_mode(0o600));
    let mut shutdown = Some(shutdown);
    loop {
        let (stream, _) = listener.accept().await?;
        let controller = controller.clone();
        let mut shutdown_sender = None;
        let mut reader = AsyncBufReader::new(stream);
        let mut line = String::new();
        reader.read_line(&mut line).await?;
        let result = handle_command(&line, &controller).await;
        let response = match result {
            Ok(CommandResult::Text(text)) => text,
            Ok(CommandResult::Shutdown(text)) => {
                shutdown_sender = Some(());
                text
            }
            Err(error) => format!("error: {error}"),
        };
        let mut stream = reader.into_inner();
        stream.write_all(response.as_bytes()).await?;
        stream.write_all(b"\n").await?;
        stream.shutdown().await?;
        if shutdown_sender.is_some() {
            if let Some(sender) = shutdown.take() {
                let _ = sender.send(());
            }
            break;
        }
    }
    let _ = fs::remove_file(&socket_path);
    Ok(())
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
    let config = load_config(&config_path)?;
    let controller = Arc::new(Mutex::new(Controller {
        config_path,
        config,
        runtime: Runtime::default(),
    }));
    {
        let mut guard = controller.lock().await;
        guard.start().await.map_err(boxed_error)?;
    }
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
    let socket_for_cleanup = socket_path.clone();
    let mut control = tokio::spawn(control_server(socket_path, controller.clone(), shutdown_tx));
    tokio::select! {
        result = &mut control => {
            result??;
        }
        _ = shutdown_rx => {}
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
            test_rule("tcp", DEFAULT_UDP_TIMEOUT),
            listener,
            target_addr,
            Arc::new(Semaphore::new(8)),
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
