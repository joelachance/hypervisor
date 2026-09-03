#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::collections::HashSet;
use std::fs;
use std::path::PathBuf;
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

use objc2::runtime::AnyObject;
use objc2::{class, msg_send, sel};
use objc2_foundation::{NSEdgeInsets, NSRect};
use tauri::Emitter;
use tauri::{AppHandle, LogicalPosition, LogicalSize, Manager};
use tauri_plugin_global_shortcut::ShortcutState;

const MARGIN: f64 = 14.0;
const TOGGLE_SHORTCUT: &str = "cmd+shift+space";

// ---------------------------------------------------------------------------
// Overlay geometry
// ---------------------------------------------------------------------------

/// Notch cutout in window-local CSS px: left/right x edges and depth from the
/// window's top, margin already applied.
#[derive(Clone, serde::Serialize)]
struct Cutout {
    left: f64,
    right: f64,
    depth: f64,
}

struct Geometry {
    cutout: Option<Cutout>,
}

#[tauri::command]
fn geometry(state: tauri::State<Geometry>) -> Option<Cutout> {
    state.cutout.clone()
}

#[tauri::command]
fn toggle(app: AppHandle) {
    toggle_overlay(&app);
}

#[tauri::command]
fn hide_overlay(app: AppHandle) {
    let Some(win) = app.get_webview_window("main") else {
        return;
    };
    let _ = win.emit("overlay-hide", ());
    let _ = win.hide();
    if let Some(l) = *LAYOUT.lock().unwrap() {
        apply_layout(&win, l, false);
    }
}

/// The overlay's canonical frame, computed once at setup. Every show/hide/
/// expand re-asserts position AND size from this, because macOS anchors a
/// hidden window's resize to its bottom edge and lets the position drift.
#[derive(Clone, Copy)]
struct Layout {
    x: f64,
    y: f64,
    width: f64,
    half: f64,
    full: f64,
    /// NSWindow level, re-asserted on every show in case anything resets it.
    level: isize,
}

static LAYOUT: std::sync::Mutex<Option<Layout>> = std::sync::Mutex::new(None);

fn apply_layout(win: &tauri::WebviewWindow, l: Layout, full: bool) {
    let _ = win.set_position(LogicalPosition::new(l.x, l.y));
    let _ = win.set_size(LogicalSize::new(
        l.width,
        if full { l.full } else { l.half },
    ));
}

/// Ask the page to mask itself to zero height. It calls `reveal_ready` once
/// the mask has painted, and only then does the window become visible — so a
/// stale unmasked frame can never flash on screen.
fn request_show(win: &tauri::WebviewWindow) {
    let _ = win.emit("overlay-show", ());
}

#[tauri::command]
fn reveal_ready(app: AppHandle) {
    let Some(win) = app.get_webview_window("main") else {
        return;
    };
    let Some(l) = *LAYOUT.lock().unwrap() else {
        return;
    };
    apply_layout(&win, l, false);
    if let Ok(ns_win) = win.ns_window() {
        let ns_win = ns_win as *mut AnyObject;
        unsafe {
            let _: () = msg_send![ns_win, setLevel: l.level];
            // Shows without activating the app; keyboard focus stays put.
            let _: () = msg_send![ns_win, orderFrontRegardless];
        }
    }
}

static LAST_TOGGLE: std::sync::Mutex<Option<std::time::Instant>> = std::sync::Mutex::new(None);
const DOUBLE_TAP_MS: u128 = 500;

/// Hotkey: single press toggles the half-screen overlay; a quick second tap
/// (hotkey, then space again inside the double-tap window) extends it to full
/// height. Any press while fullscreen hides it, and the next show is half.
fn toggle_overlay(app: &AppHandle) {
    let Some(win) = app.get_webview_window("main") else {
        return;
    };
    let Some(layout) = *LAYOUT.lock().unwrap() else {
        return;
    };
    let mut last = LAST_TOGGLE.lock().unwrap();
    let double_tap = last.is_some_and(|t| t.elapsed().as_millis() < DOUBLE_TAP_MS);
    *last = Some(std::time::Instant::now());
    drop(last);

    if !win.is_visible().unwrap_or(false) {
        request_show(&win);
        return;
    }
    let scale = win.scale_factor().unwrap_or(1.0);
    let current = win
        .outer_size()
        .map(|s| s.to_logical::<f64>(scale).height)
        .unwrap_or(layout.half);
    let at_full = current >= (layout.half + layout.full) / 2.0;
    if double_tap && !at_full {
        // Let the page pin its clip mask at the current bottom edge before
        // the window grows, so the expansion reveals instead of snapping.
        let _ = win.emit("overlay-expand", ());
        let win = win.clone();
        std::thread::spawn(move || {
            std::thread::sleep(std::time::Duration::from_millis(50));
            apply_layout(&win, layout, true);
        });
    } else {
        let _ = win.emit("overlay-hide", ());
        let _ = win.hide();
        apply_layout(&win, layout, false);
    }
}

/// (notch left x, notch right x, notch depth) in logical points from the
/// primary screen's top-left. None on screens without a notch.
unsafe fn detect_notch() -> Option<(f64, f64, f64)> {
    let screens: *mut AnyObject = msg_send![class!(NSScreen), screens];
    let screen: *mut AnyObject = msg_send![screens, firstObject];
    if screen.is_null() {
        return None;
    }
    let responds: bool = msg_send![screen, respondsToSelector: sel!(safeAreaInsets)];
    if !responds {
        return None;
    }
    let insets: NSEdgeInsets = msg_send![screen, safeAreaInsets];
    if insets.top <= 1.0 {
        return None;
    }
    let frame: NSRect = msg_send![screen, frame];
    let aux: bool = msg_send![screen, respondsToSelector: sel!(auxiliaryTopLeftArea)];
    if aux {
        let left: NSRect = msg_send![screen, auxiliaryTopLeftArea];
        let right: NSRect = msg_send![screen, auxiliaryTopRightArea];
        Some((
            left.origin.x + left.size.width - frame.origin.x,
            right.origin.x - frame.origin.x,
            insets.top,
        ))
    } else {
        // Notched screen but no per-side API: assume a 200pt centered notch.
        let notch_w = 200.0;
        Some((
            (frame.size.width - notch_w) / 2.0,
            (frame.size.width + notch_w) / 2.0,
            insets.top,
        ))
    }
}

// ---------------------------------------------------------------------------
// tmux sessions
// ---------------------------------------------------------------------------

#[derive(Clone, serde::Serialize, serde::Deserialize)]
struct Session {
    name: String,
    provider: String,
    model: String,
    label: String,
    prompt: String,
    created_at: u64,
    #[serde(default)]
    cwd: String,
    /// "stream" = headless Claude Code stream-json driver; "tui" = pane scrape.
    #[serde(default)]
    driver: String,
    /// Claude Code session id, for --resume across turns (stream driver).
    #[serde(default)]
    session_id: String,
    #[serde(default)]
    alive: bool,
    /// A turn is currently executing (its tmux pane is live).
    #[serde(default)]
    running: bool,
    /// Agent exited but the tmux pane is retained (remain-on-exit).
    #[serde(default)]
    done: bool,
    #[serde(default)]
    adds: u32,
    #[serde(default)]
    dels: u32,
    #[serde(default)]
    branch: String,
    #[serde(default)]
    repo: String,
}

/// Repo name a directory belongs to: the directory holding the shared .git,
/// so sessions in worktrees group under their repo.
fn repo_name(cwd: &str) -> String {
    let fallback = || {
        PathBuf::from(cwd)
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default()
    };
    match git(cwd, &["rev-parse", "--git-common-dir"]) {
        Some(dir) if dir != ".git" && !dir.is_empty() => PathBuf::from(dir)
            .parent()
            .and_then(|p| p.file_name())
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(fallback),
        Some(_) => fallback(),
        None => fallback(),
    }
}

/// One rendered transcript item. kind: "user" | "assistant" | "tool".
#[derive(serde::Serialize, Clone)]
struct Ev {
    kind: String,
    text: String,
}

#[derive(serde::Serialize)]
struct Peek {
    session: Session,
    events: Vec<Ev>,
}

fn stream_path(name: &str) -> PathBuf {
    sessions_dir().join(format!("{name}.jsonl"))
}

/// Append our own user-prompt event to the stream log. Claude's stream-json
/// does not echo the human prompt, so we record it ourselves to keep the
/// transcript (and turn ordering) complete.
fn append_user_prompt(name: &str, text: &str) {
    let line = serde_json::json!({ "type": "user_prompt", "text": text }).to_string();
    if let Ok(mut f) = fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(stream_path(name))
    {
        use std::io::Write;
        let _ = writeln!(f, "{line}");
    }
}

/// The resumable session id from a stream log, per provider. Claude puts it
/// in the init event's `session_id`; Codex in the `thread.started` event's
/// `thread_id`.
fn read_session_id(name: &str, provider: &str) -> Option<String> {
    let text = fs::read_to_string(stream_path(name)).ok()?;
    for line in text.lines() {
        let Ok(v) = serde_json::from_str::<serde_json::Value>(line) else {
            continue;
        };
        let ty = v.get("type").and_then(|t| t.as_str());
        if provider == "openai" {
            if ty == Some("thread.started") {
                if let Some(id) = v.get("thread_id").and_then(|s| s.as_str()) {
                    return Some(id.to_string());
                }
            }
        } else if ty == Some("system")
            && v.get("subtype").and_then(|s| s.as_str()) == Some("init")
        {
            if let Some(id) = v.get("session_id").and_then(|s| s.as_str()) {
                return Some(id.to_string());
            }
        }
    }
    None
}

/// A short human label for a tool call from its input JSON.
fn tool_summary(name: &str, input: &serde_json::Value) -> String {
    let pick = |keys: &[&str]| -> Option<String> {
        keys.iter().find_map(|k| {
            input
                .get(k)
                .and_then(|v| v.as_str())
                .map(|s| s.to_string())
        })
    };
    let detail = pick(&[
        "command",
        "file_path",
        "path",
        "pattern",
        "query",
        "url",
        "description",
        "prompt",
    ])
    .unwrap_or_default();
    let detail = detail.replace('\n', " ");
    let detail = if detail.chars().count() > 72 {
        format!("{}…", detail.chars().take(72).collect::<String>())
    } else {
        detail
    };
    let tool = name.to_lowercase();
    if detail.is_empty() {
        tool
    } else {
        format!("{tool}  {detail}")
    }
}

/// Parse a Claude stream-json log into transcript events.
fn parse_stream(text: &str) -> Vec<Ev> {
    let mut events = Vec::new();
    for line in text.lines() {
        let Ok(v) = serde_json::from_str::<serde_json::Value>(line) else {
            continue;
        };
        match v.get("type").and_then(|t| t.as_str()) {
            Some("user_prompt") => {
                if let Some(t) = v.get("text").and_then(|t| t.as_str()) {
                    events.push(Ev { kind: "user".into(), text: t.to_string() });
                }
            }
            Some("assistant") => {
                let content = v
                    .get("message")
                    .and_then(|m| m.get("content"))
                    .and_then(|c| c.as_array());
                for item in content.into_iter().flatten() {
                    match item.get("type").and_then(|t| t.as_str()) {
                        Some("text") => {
                            if let Some(t) = item.get("text").and_then(|t| t.as_str()) {
                                if !t.trim().is_empty() {
                                    events.push(Ev { kind: "assistant".into(), text: t.to_string() });
                                }
                            }
                        }
                        Some("tool_use") => {
                            let name = item.get("name").and_then(|n| n.as_str()).unwrap_or("tool");
                            let empty = serde_json::json!({});
                            let input = item.get("input").unwrap_or(&empty);
                            events.push(Ev { kind: "tool".into(), text: tool_summary(name, input) });
                        }
                        _ => {}
                    }
                }
            }
            _ => {} // tool_result, system, result, rate_limit — not rendered
        }
    }
    // Keep the last few exchanges: cap to the trailing 24 events.
    if events.len() > 24 {
        events = events.split_off(events.len() - 24);
    }
    events
}

/// Trim a value to a single line and cap its length for a tool label.
fn short(detail: &str) -> String {
    let detail = detail.replace('\n', " ");
    let detail = detail.trim();
    if detail.chars().count() > 72 {
        format!("{}…", detail.chars().take(72).collect::<String>())
    } else {
        detail.to_string()
    }
}

/// Parse a Codex `exec --json` log into transcript events.
fn parse_codex_stream(text: &str) -> Vec<Ev> {
    let mut events = Vec::new();
    for line in text.lines() {
        let Ok(v) = serde_json::from_str::<serde_json::Value>(line) else {
            continue;
        };
        match v.get("type").and_then(|t| t.as_str()) {
            Some("user_prompt") => {
                if let Some(t) = v.get("text").and_then(|t| t.as_str()) {
                    events.push(Ev { kind: "user".into(), text: t.to_string() });
                }
            }
            Some("item.completed") => {
                let Some(item) = v.get("item") else { continue };
                match item.get("type").and_then(|t| t.as_str()) {
                    Some("agent_message") => {
                        if let Some(t) = item.get("text").and_then(|t| t.as_str()) {
                            if !t.trim().is_empty() {
                                events.push(Ev { kind: "assistant".into(), text: t.to_string() });
                            }
                        }
                    }
                    Some("command_execution") => {
                        // Strip the "/bin/zsh -lc '…'" shell wrapper if present.
                        let cmd = item.get("command").and_then(|c| c.as_str()).unwrap_or("");
                        let inner = cmd
                            .split_once("-lc ")
                            .map(|(_, r)| r.trim().trim_matches('\''))
                            .unwrap_or(cmd);
                        events.push(Ev { kind: "tool".into(), text: format!("bash  {}", short(inner)) });
                    }
                    Some("file_change") => {
                        let path = item
                            .get("path")
                            .or_else(|| item.get("file_path"))
                            .and_then(|p| p.as_str())
                            .unwrap_or("");
                        events.push(Ev { kind: "tool".into(), text: format!("edit  {}", short(path)) });
                    }
                    // Skip Codex's noisy warning items and internal reasoning.
                    Some("error") | Some("reasoning") | None => {}
                    Some(other) => {
                        events.push(Ev { kind: "tool".into(), text: other.replace('_', " ") });
                    }
                }
            }
            _ => {} // thread.started, turn.*, usage — not rendered
        }
    }
    if events.len() > 24 {
        events = events.split_off(events.len() - 24);
    }
    events
}

/// Parse a stream log into events, dispatching on provider.
fn parse_stream_for(text: &str, provider: &str) -> Vec<Ev> {
    if provider == "openai" {
        parse_codex_stream(text)
    } else {
        parse_stream(text)
    }
}

/// Convert scraped pane blocks (tui driver) into the same event shape.
fn blocks_to_events(blocks: &[String]) -> Vec<Ev> {
    let mut events = Vec::new();
    for block in blocks {
        for line in block.lines() {
            let t = line.trim();
            if t.is_empty() {
                continue;
            }
            if let Some(rest) = prompt_text(line) {
                events.push(Ev { kind: "user".into(), text: rest.to_string() });
            } else if let Some(rest) = t.strip_prefix("⏺ ") {
                events.push(Ev { kind: "assistant".into(), text: rest.to_string() });
            } else {
                events.push(Ev { kind: "assistant".into(), text: t.to_string() });
            }
        }
    }
    events
}

fn sessions_dir() -> PathBuf {
    let home = std::env::var("HOME").unwrap_or_else(|_| "/tmp".into());
    PathBuf::from(home).join(".hypervisor").join("sessions")
}

/// Single-quote a string for POSIX shells.
fn shq(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

/// Dedicated tmux server socket, so every hypervisor process — across
/// rebuilds and regardless of inherited env — shares one server and sees the
/// same sessions. Without this, a new app instance can bind a different
/// default socket and find none of the running sessions.
const TMUX_SOCKET: &str = "hypervisor";

fn tmux(args: &[&str]) -> Result<String, String> {
    let out = Command::new("tmux")
        .arg("-L")
        .arg(TMUX_SOCKET)
        .args(args)
        .output()
        .map_err(|e| format!("tmux: {e}"))?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    } else {
        Err(String::from_utf8_lossy(&out.stderr).trim().to_string())
    }
}

/// session name -> pane_dead for every tmux session. The format puts the
/// single-char pane_dead flag first so parsing needs no delimiter (a literal
/// tab in the -F string does not survive to the output on this tmux).
fn pane_states() -> std::collections::HashMap<String, bool> {
    tmux(&["list-panes", "-a", "-F", "#{pane_dead}#{session_name}"])
        .map(|out| {
            out.lines()
                .filter_map(|l| {
                    let mut chars = l.chars();
                    let dead = chars.next()? == '1';
                    let name = chars.as_str();
                    if name.is_empty() {
                        return None;
                    }
                    Some((name.to_string(), dead))
                })
                .collect()
        })
        .unwrap_or_default()
}

fn valid_name(name: &str) -> bool {
    !name.is_empty() && name.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
}

fn git(cwd: &str, args: &[&str]) -> Option<String> {
    let out = Command::new("git")
        .arg("-C")
        .arg(cwd)
        .args(args)
        .output()
        .ok()?;
    if out.status.success() {
        Some(String::from_utf8_lossy(&out.stdout).trim().to_string())
    } else {
        None
    }
}

fn tilde(path: &str) -> String {
    let home = std::env::var("HOME").unwrap_or_default();
    if !home.is_empty() && path.starts_with(&home) {
        format!("~{}", &path[home.len()..])
    } else {
        path.to_string()
    }
}

/// The default working directory for new sessions and the status bar,
/// settable from the /cd panel.
static DEFAULT_CWD: std::sync::Mutex<Option<String>> = std::sync::Mutex::new(None);

fn default_cwd() -> String {
    if let Some(c) = DEFAULT_CWD.lock().unwrap().clone() {
        return c;
    }
    let home = std::env::var("HOME").unwrap_or_else(|_| "/tmp".into());
    let repo = format!("{home}/git/hv");
    if PathBuf::from(&repo).is_dir() {
        repo
    } else {
        home
    }
}

#[tauri::command]
fn set_cwd(path: String) -> Result<(), String> {
    if !PathBuf::from(&path).is_dir() {
        return Err("not a directory".into());
    }
    *DEFAULT_CWD.lock().unwrap() = Some(path);
    Ok(())
}

fn slug_ok(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 64
        && s.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '/'))
}

/// Create a git worktree off the current repo on a new branch, and switch the
/// default working directory to it so the next session opens there.
#[tauri::command]
fn create_worktree(name: String) -> Result<String, String> {
    let name = name.trim().to_string();
    if !slug_ok(&name) {
        return Err("use letters, digits, - _ /".into());
    }
    let cwd = default_cwd();
    let common = git(&cwd, &["rev-parse", "--path-format=absolute", "--git-common-dir"])
        .ok_or("not a git repo")?;
    let repo_root = PathBuf::from(&common)
        .parent()
        .ok_or("no repo root")?
        .to_path_buf();
    let parent = repo_root.parent().ok_or("no parent dir")?;
    let leaf = name.rsplit('/').next().unwrap_or(&name);
    let wt_path = parent.join(format!("{}-{}", repo_name(&cwd), leaf));
    let wt_str = wt_path.to_string_lossy().into_owned();
    if wt_path.exists() {
        return Err("worktree path already exists".into());
    }
    git(
        &repo_root.to_string_lossy(),
        &["worktree", "add", &wt_str, "-b", &name],
    )
    .ok_or("git worktree add failed")?;
    *DEFAULT_CWD.lock().unwrap() = Some(wt_str.clone());
    Ok(wt_str)
}

/// Create (or switch to) a branch in the current working directory.
#[tauri::command]
fn switch_branch(name: String) -> Result<(), String> {
    let name = name.trim().to_string();
    if !slug_ok(&name) {
        return Err("use letters, digits, - _ /".into());
    }
    let cwd = default_cwd();
    // -B creates or resets-to; use switch -c, fall back to switch.
    if git(&cwd, &["switch", "-c", &name]).is_some() {
        return Ok(());
    }
    git(&cwd, &["switch", &name])
        .map(|_| ())
        .ok_or_else(|| "could not switch branch".into())
}

/// The working diff of the current directory, capped for display.
#[tauri::command]
fn working_diff() -> String {
    let cwd = default_cwd();
    let out = git(&cwd, &["diff", "--stat", "HEAD"]).unwrap_or_default();
    let full = git(&cwd, &["diff", "HEAD"]).unwrap_or_default();
    let body: String = full.lines().take(400).collect::<Vec<_>>().join("\n");
    if out.trim().is_empty() && body.trim().is_empty() {
        "no working changes".into()
    } else {
        format!("{out}\n\n{body}")
    }
}

fn diffstat(cwd: &str) -> (u32, u32) {
    let Some(out) = git(cwd, &["diff", "--numstat", "HEAD"]) else {
        return (0, 0);
    };
    let mut adds = 0u32;
    let mut dels = 0u32;
    for line in out.lines() {
        let mut cols = line.split_whitespace();
        adds += cols.next().and_then(|c| c.parse::<u32>().ok()).unwrap_or(0);
        dels += cols.next().and_then(|c| c.parse::<u32>().ok()).unwrap_or(0);
    }
    (adds, dels)
}

#[derive(serde::Serialize)]
struct StatusInfo {
    cwd: String,
    branch: String,
    target: String,
    worktree: Option<String>,
    cpu: u32,
}

#[tauri::command]
fn status_info(cwd: Option<String>) -> StatusInfo {
    let cwd = cwd.filter(|c| !c.is_empty()).unwrap_or_else(default_cwd);
    let branch = git(&cwd, &["rev-parse", "--abbrev-ref", "HEAD"]).unwrap_or_else(|| "-".into());
    let git_dir = git(&cwd, &["rev-parse", "--absolute-git-dir"]);
    let common = git(&cwd, &["rev-parse", "--git-common-dir"]);
    let worktree = match (&git_dir, &common) {
        (Some(g), Some(c)) if g != c && !c.is_empty() => PathBuf::from(&cwd)
            .file_name()
            .map(|n| n.to_string_lossy().into_owned()),
        _ => None,
    };
    let cpu = Command::new("sh")
        .arg("-c")
        .arg("ps -A -o %cpu= | awk -v n=$(sysctl -n hw.ncpu) '{s+=$1} END {printf \"%d\", s/n}'")
        .output()
        .ok()
        .and_then(|o| String::from_utf8_lossy(&o.stdout).trim().parse::<u32>().ok())
        .unwrap_or(0);
    StatusInfo {
        cwd: tilde(&cwd),
        branch,
        target: "main".into(),
        worktree,
        cpu,
    }
}

#[derive(serde::Serialize)]
struct DirEntry {
    path: String,
    display: String,
    kind: String, // "current" | "worktree" | ""
    files: u32,
    modified: u64,
}

#[tauri::command]
fn list_dirs() -> Vec<DirEntry> {
    let home = std::env::var("HOME").unwrap_or_default();
    let current = default_cwd();
    let mut seen = HashSet::new();
    let mut out = Vec::new();

    let push = |path: String, kind: &str, out: &mut Vec<DirEntry>, seen: &mut HashSet<String>| {
        if !seen.insert(path.clone()) {
            return;
        }
        let Ok(meta) = fs::metadata(&path) else { return };
        let files = fs::read_dir(&path).map(|rd| rd.count() as u32).unwrap_or(0);
        let modified = meta
            .modified()
            .ok()
            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
            .map(|d| d.as_secs())
            .unwrap_or(0);
        out.push(DirEntry {
            display: tilde(&path),
            path,
            kind: kind.into(),
            files,
            modified,
        });
    };

    push(current.clone(), "current", &mut out, &mut seen);
    // Worktrees of the current repo.
    if let Some(list) = git(&current, &["worktree", "list", "--porcelain"]) {
        for line in list.lines() {
            if let Some(p) = line.strip_prefix("worktree ") {
                let kind = if p == current { "current" } else { "worktree" };
                push(p.to_string(), kind, &mut out, &mut seen);
            }
        }
    }
    // One level under ~/git.
    if let Ok(rd) = fs::read_dir(format!("{home}/git")) {
        for entry in rd.flatten() {
            let p = entry.path();
            if p.is_dir() {
                push(p.to_string_lossy().into_owned(), "", &mut out, &mut seen);
            }
        }
    }
    // Session directories.
    for s in list_sessions() {
        if !s.cwd.is_empty() {
            push(s.cwd, "", &mut out, &mut seen);
        }
    }
    out
}

#[tauri::command]
fn start_session(
    provider: String,
    model: String,
    label: String,
    prompt: String,
) -> Result<Session, String> {
    if prompt.trim().is_empty() {
        return Err("prompt is empty".into());
    }
    let created = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let name = format!("hv-{created}");
    let cwd = default_cwd();
    // Anthropic and OpenAI run through headless structured drivers (exact
    // transcript + native subagents); other providers keep the TUI path.
    let driver = if provider == "anthropic" || provider == "openai" {
        "stream"
    } else {
        "tui"
    };
    let jsonl = stream_path(&name);
    let jsonl_s = jsonl.to_string_lossy();

    let agent_cmd = match provider.as_str() {
        "anthropic" => format!(
            "claude -p {} --model {} --output-format stream-json --verbose >> {} 2>&1",
            shq(&prompt),
            shq(&model),
            shq(&jsonl_s)
        ),
        "openai" => format!(
            "codex exec --json --dangerously-bypass-approvals-and-sandbox -m {} {} >> {} 2>&1",
            shq(&model),
            shq(&prompt),
            shq(&jsonl_s)
        ),
        _ => format!("opencode --model {} --prompt {}", shq(&model), shq(&prompt)),
    };
    // The tmux server may have been spawned with a bare environment; carry
    // this app's PATH (nvm, homebrew, …) into the session explicitly.
    let path = std::env::var("PATH").unwrap_or_default();
    let cmd = format!("export PATH={}; exec {agent_cmd}", shq(&path));

    if driver == "stream" {
        append_user_prompt(&name, &prompt);
    }
    tmux(&["new-session", "-d", "-s", &name, "-c", &cwd, &cmd])?;
    // Keep the pane readable after the agent exits, so sessions that finish
    // in the background don't vanish with their transcript.
    let _ = tmux(&["set-option", "-t", &name, "remain-on-exit", "on"]);

    let repo = repo_name(&cwd);
    let session = Session {
        name: name.clone(),
        provider,
        model,
        label,
        prompt,
        created_at: created,
        cwd,
        driver: driver.into(),
        session_id: String::new(),
        alive: true,
        running: true,
        done: false,
        adds: 0,
        dels: 0,
        branch: String::new(),
        repo,
    };
    let dir = sessions_dir();
    fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    fs::write(
        dir.join(format!("{name}.json")),
        serde_json::to_vec_pretty(&session).map_err(|e| e.to_string())?,
    )
    .map_err(|e| e.to_string())?;
    Ok(session)
}

#[tauri::command]
fn list_sessions() -> Vec<Session> {
    let states = pane_states();
    let mut sessions: Vec<Session> = fs::read_dir(sessions_dir())
        .map(|rd| {
            rd.filter_map(|entry| {
                let path = entry.ok()?.path();
                if path.extension()? != "json" {
                    return None;
                }
                let mut s: Session = serde_json::from_slice(&fs::read(path).ok()?).ok()?;
                let pane_alive = states.get(&s.name).copied() == Some(false);
                let pane_dead = states.get(&s.name).copied() == Some(true);
                if s.driver == "stream" {
                    // A stream session is a resumable conversation: alive as
                    // long as its log exists, running only while a turn's pane
                    // is live, and it never "finishes".
                    s.running = pane_alive;
                    s.alive = stream_path(&s.name).exists();
                    s.done = false;
                    if s.session_id.is_empty() {
                        s.session_id = read_session_id(&s.name, &s.provider).unwrap_or_default();
                    }
                } else {
                    s.running = pane_alive;
                    s.alive = pane_alive;
                    s.done = pane_dead;
                }
                if s.cwd.is_empty() {
                    s.cwd = default_cwd();
                }
                if s.alive {
                    let (adds, dels) = diffstat(&s.cwd);
                    s.adds = adds;
                    s.dels = dels;
                }
                s.branch = git(&s.cwd, &["rev-parse", "--abbrev-ref", "HEAD"]).unwrap_or_default();
                s.repo = repo_name(&s.cwd);
                Some(s)
            })
            .collect()
        })
        .unwrap_or_default();
    sessions.sort_by(|a, b| b.created_at.cmp(&a.created_at));
    sessions
}

#[tauri::command]
fn peek_session(name: String) -> Result<Peek, String> {
    if !valid_name(&name) {
        return Err("invalid session name".into());
    }
    let raw = fs::read(sessions_dir().join(format!("{name}.json"))).map_err(|e| e.to_string())?;
    let mut session: Session = serde_json::from_slice(&raw).map_err(|e| e.to_string())?;
    let pane = pane_states().get(&name).copied();
    let pane_alive = pane == Some(false);

    let events = if session.driver == "stream" {
        session.running = pane_alive;
        session.alive = stream_path(&name).exists();
        session.done = false;
        if session.session_id.is_empty() {
            session.session_id = read_session_id(&name, &session.provider).unwrap_or_default();
        }
        let text = fs::read_to_string(stream_path(&name)).unwrap_or_default();
        parse_stream_for(&text, &session.provider)
    } else {
        session.running = pane_alive;
        session.alive = pane_alive;
        session.done = pane == Some(true);
        let blocks = if pane.is_some() {
            let text = tmux(&["capture-pane", "-p", "-t", &name, "-S", "-400"]).unwrap_or_default();
            parse_blocks(&text)
        } else {
            Vec::new()
        };
        blocks_to_events(&blocks)
    };
    Ok(Peek { session, events })
}

#[tauri::command]
fn send_prompt(name: String, text: String) -> Result<(), String> {
    if !valid_name(&name) {
        return Err("invalid session name".into());
    }
    let text = text.replace(['\n', '\r'], " ").trim().to_string();
    if text.is_empty() {
        return Err("empty prompt".into());
    }

    let raw = fs::read(sessions_dir().join(format!("{name}.json"))).map_err(|e| e.to_string())?;
    let session: Session = serde_json::from_slice(&raw).map_err(|e| e.to_string())?;

    if session.driver == "stream" {
        // A stream turn runs to completion and exits; only one at a time.
        if pane_states().get(&name).copied() == Some(false) {
            return Err("a turn is still running".into());
        }
        let session_id = read_session_id(&name, &session.provider).ok_or("session not ready yet")?;
        let jsonl = stream_path(&name);
        let jsonl_s = jsonl.to_string_lossy();
        let agent = if session.provider == "openai" {
            format!(
                "codex exec resume {} {} --json --dangerously-bypass-approvals-and-sandbox >> {} 2>&1",
                shq(&session_id),
                shq(&text),
                shq(&jsonl_s)
            )
        } else {
            format!(
                "claude -p {} --resume {} --model {} --output-format stream-json --verbose >> {} 2>&1",
                shq(&text),
                shq(&session_id),
                shq(&session.model),
                shq(&jsonl_s)
            )
        };
        let path = std::env::var("PATH").unwrap_or_default();
        let cmd = format!("export PATH={}; exec {agent}", shq(&path));
        append_user_prompt(&name, &text);
        // Reuse the session name: clear the retired (dead) pane first.
        let _ = tmux(&["kill-session", "-t", &name]);
        tmux(&["new-session", "-d", "-s", &name, "-c", &session.cwd, &cmd])?;
        let _ = tmux(&["set-option", "-t", &name, "remain-on-exit", "on"]);
        return Ok(());
    }

    // TUI driver: type into the live pane.
    match pane_states().get(&name).copied() {
        Some(false) => {}
        Some(true) => return Err("session has finished".into()),
        None => return Err("session is not running".into()),
    }
    tmux(&["send-keys", "-t", &name, "-l", "--", &text])?;
    std::thread::sleep(std::time::Duration::from_millis(120));
    tmux(&["send-keys", "-t", &name, "Enter"]).map(|_| ())
}

#[tauri::command]
fn kill_session(name: String) -> Result<(), String> {
    if !valid_name(&name) {
        return Err("invalid session name".into());
    }
    tmux(&["kill-session", "-t", &name]).map(|_| ())
}

/// True for a line that is Claude/agent TUI chrome, not transcript content:
/// the startup banner (block/braille art, version and model lines), the
/// spinner/status line, the input hints, and rule separators.
fn is_chrome(line: &str) -> bool {
    let t = line.trim();
    if t.is_empty() {
        return false; // blank lines are handled by segmentation, not dropped here
    }
    // Bare input prompt (the empty box), no text after the marker.
    if matches!(t, "❯" | ">" | "›") {
        return true;
    }
    // Rule separators around the input box.
    if t.chars().all(|c| matches!(c, '─' | '━' | '═' | '-')) && t.chars().count() > 3 {
        return true;
    }
    // Startup banner art (block-drawing / braille glyphs). The version, model
    // and cwd lines each carry these glyphs, so this clears the whole banner.
    if t.chars()
        .any(|c| matches!(c, '\u{2580}'..='\u{259F}' | '\u{2800}'..='\u{28FF}'))
    {
        return true;
    }
    let markers = [
        "Claude Code v",
        "· Claude",
        "Claude Max",
        "Claude Pro",
        "Updated to latest",
        "code.claude.com",
        "? for shortcuts",
        "esc to interrupt",
        "shift+tab to cycle",
        "-- INSERT --",
        "-- NORMAL --",
        "auto mode on",
        "for agents",
    ];
    if markers.iter().any(|m| t.contains(m)) {
        return true;
    }
    // Spinner / status line: "✻ Cooked for 3s · done 10:52 PM".
    t.starts_with('✻') || t.starts_with("● ")
}

/// Is this a user-prompt line ("> …", "❯ …", "› …")? Requires text after the
/// marker so a bare trailing input prompt is not treated as one.
fn prompt_text(line: &str) -> Option<&str> {
    let t = line.trim_start();
    for marker in ["> ", "❯ ", "› "] {
        if let Some(rest) = t.strip_prefix(marker) {
            let rest = rest.trim();
            if !rest.is_empty() {
                return Some(rest);
            }
        }
    }
    None
}

/// Slice a captured pane into per-exchange blocks (a user prompt line plus the
/// response that follows it) and keep the last three. Everything before the
/// first prompt (the startup banner) is discarded.
fn parse_blocks(text: &str) -> Vec<String> {
    let cleaned: Vec<&str> = text
        .lines()
        .filter(|l| !is_chrome(l))
        .collect();

    let mut blocks: Vec<Vec<&str>> = Vec::new();
    let mut cur: Vec<&str> = Vec::new();
    for &l in &cleaned {
        let is_prompt = prompt_text(l).is_some();
        if is_prompt && !cur.is_empty() {
            blocks.push(std::mem::take(&mut cur));
        }
        // Only start collecting once we've seen the first real prompt, so the
        // banner and any pre-prompt noise never become a block.
        if is_prompt || !cur.is_empty() || !blocks.is_empty() {
            cur.push(l);
        }
    }
    if !cur.is_empty() {
        blocks.push(cur);
    }
    // No prompt was ever seen (non-claude CLI, or output-only): fall back to
    // the tail of the cleaned lines so the transcript isn't blank.
    if blocks.is_empty() {
        let non_empty: Vec<&str> = cleaned.iter().copied().filter(|l| !l.trim().is_empty()).collect();
        if !non_empty.is_empty() {
            blocks.push(non_empty);
        }
    }

    let mut out: Vec<String> = blocks
        .into_iter()
        .map(|b| {
            let tail: Vec<&str> = b.iter().rev().take(40).rev().copied().collect();
            tail.join("\n").trim_matches(|c| c == '\n' || c == ' ').to_string()
        })
        .filter(|b| {
            let t = b.trim();
            !t.is_empty() && t != "❯" && t != ">" && t != "›"
        })
        .collect();
    if out.len() > 3 {
        out = out.split_off(out.len() - 3);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::{git, parse_blocks};

    // Verbatim capture of a real `claude` pane from the smoke test.
    const PANE: &str = "\
 ▐▛███▛█   Claude Code v2.1.250
▝▜██████▀  Sonnet 5 · Claude Max
  ▝▝ ▝▝    ~/git/hv
  Updated to latest. Got 4 features, 19 bugfixes, and 10 other changes.
❯ Respond with exactly the text HV_TMUX_OK and nothing else, then wait for
  further instructions.
⏺ HV_TMUX_OK
✻ Brewed for 1s · done 8:32 PM
                                                              ● high · /effort
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents         /rc
";

    #[test]
    fn parses_claude_pane_into_exchanges() {
        let blocks = parse_blocks(PANE);
        let last = blocks.last().expect("no blocks parsed");
        assert!(last.contains("HV_TMUX_OK"), "response missing: {last:?}");
        assert!(last.starts_with('❯'), "block should start at the prompt");
        for b in &blocks {
            assert!(!b.contains("-- INSERT --"), "status line leaked: {b:?}");
            assert!(!b.contains("────"), "separator leaked: {b:?}");
        }
        // The bare trailing input prompt must not become its own block.
        assert!(blocks.iter().all(|b| b.trim() != "❯"));
    }

    // The exact pane the app captured live (banner + spinner + trailing prompt).
    const LIVE_PANE: &str = "\
 ▐▛███▛█   Claude Code v2.1.252
▝▜██████▀  Sonnet 5 · Claude Max
  ▝▝ ▝▝    ~/git/hv
❯ Reply with exactly HV_LIVE_OK
⏺ HV_LIVE_OK
✻ Cooked for 3s · done 10:52 PM
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents         /rc
";

    #[test]
    fn strips_all_claude_chrome() {
        let blocks = parse_blocks(LIVE_PANE);
        assert_eq!(blocks.len(), 1, "should be exactly one exchange: {blocks:?}");
        let b = &blocks[0];
        // Only the prompt and the response survive.
        assert_eq!(b, "❯ Reply with exactly HV_LIVE_OK\n⏺ HV_LIVE_OK");
        // No banner, model line, cwd echo, spinner, or box art anywhere.
        for needle in ["Claude Code", "Claude Max", "Sonnet", "git/hv", "✻", "Cooked", "▐", "▜", "──", "INSERT"] {
            assert!(!b.contains(needle), "chrome leaked ({needle}): {b:?}");
        }
    }

    #[test]
    fn pane_state_line_parses_flag_first() {
        // Format is "#{pane_dead}#{session_name}": leading 0/1 then the name.
        let parse = |l: &str| -> Option<(String, bool)> {
            let mut chars = l.chars();
            let dead = chars.next()? == '1';
            let name = chars.as_str();
            if name.is_empty() {
                return None;
            }
            Some((name.to_string(), dead))
        };
        assert_eq!(parse("0hv-1788321122"), Some(("hv-1788321122".into(), false)));
        assert_eq!(parse("1hv-42"), Some(("hv-42".into(), true)));
        assert_eq!(parse(""), None);
    }

    #[test]
    fn unsegmented_output_returns_tail() {
        let text = (0..100).map(|i| format!("line {i}\n")).collect::<String>();
        let blocks = parse_blocks(&text);
        assert_eq!(blocks.len(), 1);
        assert!(blocks[0].contains("line 99"));
        assert!(!blocks[0].contains("line 10\n"), "should cap to a tail");
    }

    fn run_git(dir: &std::path::Path, args: &[&str]) {
        let ok = std::process::Command::new("git")
            .arg("-C")
            .arg(dir)
            .args(args)
            .output()
            .expect("git")
            .status
            .success();
        assert!(ok, "git {args:?} failed");
    }

    // Exercises the real /branch, /worktree, /diff command functions against a
    // throwaway repo. One test (not several) because they share the global
    // DEFAULT_CWD, which parallel tests would race.
    #[test]
    fn code_commands_drive_real_git() {
        let base = std::env::temp_dir().join(format!("hvtest-{}", std::process::id()));
        let repo = base.join("myrepo");
        std::fs::create_dir_all(&repo).unwrap();
        run_git(&repo, &["init", "-q"]);
        run_git(&repo, &["config", "user.email", "t@t"]);
        run_git(&repo, &["config", "user.name", "t"]);
        run_git(&repo, &["config", "commit.gpgsign", "false"]);
        std::fs::write(repo.join("a.txt"), "one\n").unwrap();
        run_git(&repo, &["add", "-A"]);
        run_git(&repo, &["commit", "-qm", "init"]);

        *super::DEFAULT_CWD.lock().unwrap() = Some(repo.to_string_lossy().into_owned());

        // /diff — clean, then dirty.
        assert_eq!(super::working_diff().trim(), "no working changes");
        std::fs::write(repo.join("a.txt"), "one\ntwo\n").unwrap();
        assert!(super::working_diff().contains("two"), "diff should show the change");

        // /branch — creates and switches.
        super::switch_branch("feature-x".into()).expect("switch_branch");
        let cur = git(&repo.to_string_lossy(), &["rev-parse", "--abbrev-ref", "HEAD"]).unwrap();
        assert_eq!(cur, "feature-x");

        // Commit the change so worktree add starts clean.
        run_git(&repo, &["add", "-A"]);
        run_git(&repo, &["commit", "-qm", "two"]);

        // /worktree — creates a worktree on a new branch and switches cwd to it.
        let wt = super::create_worktree("wt1".into()).expect("create_worktree");
        assert!(std::path::Path::new(&wt).is_dir(), "worktree dir exists");
        assert!(wt.ends_with("myrepo-wt1"), "worktree named <repo>-<name>: {wt}");
        let wt_branch = git(&wt, &["rev-parse", "--abbrev-ref", "HEAD"]).unwrap();
        assert_eq!(wt_branch, "wt1");
        assert_eq!(*super::DEFAULT_CWD.lock().unwrap(), Some(wt.clone()));

        // Bad names are rejected.
        assert!(super::switch_branch("bad name!".into()).is_err());
        assert!(super::create_worktree("".into()).is_err());

        let _ = std::fs::remove_dir_all(&base);
        *super::DEFAULT_CWD.lock().unwrap() = None;
    }
}

// ---------------------------------------------------------------------------

/// A GUI app launched from Finder/Spotlight inherits only a minimal PATH
/// (/usr/bin:/bin), so tmux, claude, codex and git would not resolve. Pull the
/// real PATH from a login shell and adopt it, unless we clearly already have a
/// full one (e.g. launched from a terminal / `tauri dev`).
fn adopt_login_path() {
    let have = std::env::var("PATH").unwrap_or_default();
    if have.contains("/opt/homebrew/bin") || have.contains("/.nvm/") {
        return;
    }
    let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/zsh".into());
    if let Ok(out) = Command::new(&shell)
        .args(["-lic", "printf %s \"$PATH\""])
        .output()
    {
        let path = String::from_utf8_lossy(&out.stdout);
        let path = path.trim();
        if !path.is_empty() {
            std::env::set_var("PATH", path);
        }
    }
}

fn main() {
    adopt_login_path();
    tauri::Builder::default()
        .plugin(
            tauri_plugin_global_shortcut::Builder::new()
                .with_shortcuts([TOGGLE_SHORTCUT])
                .expect("invalid shortcut")
                .with_handler(|app, _shortcut, event| {
                    if event.state() == ShortcutState::Pressed {
                        toggle_overlay(app);
                    }
                })
                .build(),
        )
        .invoke_handler(tauri::generate_handler![
            geometry,
            toggle,
            hide_overlay,
            reveal_ready,
            start_session,
            list_sessions,
            peek_session,
            send_prompt,
            kill_session,
            status_info,
            list_dirs,
            set_cwd,
            create_worktree,
            switch_branch,
            working_diff
        ])
        .setup(|app| {
            // Overlay, not an app: no Dock icon, no Cmd-Tab entry.
            app.set_activation_policy(tauri::ActivationPolicy::Accessory);

            let win = app.get_webview_window("main").expect("main window missing");
            let monitor = win
                .primary_monitor()?
                .or(win.current_monitor()?)
                .expect("no monitor");
            let scale = monitor.scale_factor();
            let screen = monitor.size().to_logical::<f64>(scale);

            let ns_win = win.ns_window()? as *mut AnyObject;
            unsafe {
                // canJoinAllSpaces | fullScreenAuxiliary: follow the user to
                // every Space, including over fullscreen apps.
                let _: () = msg_send![ns_win, setCollectionBehavior: 257usize];
            }

            let notch = unsafe { detect_notch() };
            let (layout, cutout) = if let Some((nl, nr, depth)) = notch {
                unsafe {
                    // Above the menu bar's window level, so the overlay can
                    // occupy the notch strip at the very top of the screen.
                    let _: () = msg_send![ns_win, setLevel: 25isize];
                }
                (
                    Layout {
                        x: MARGIN,
                        y: MARGIN,
                        width: screen.width - MARGIN * 2.0,
                        half: screen.height / 2.0 - MARGIN * 2.0,
                        full: screen.height - MARGIN * 2.0,
                        level: 25,
                    },
                    Some(Cutout {
                        left: nl - MARGIN * 2.0,
                        right: nr, // (nr + MARGIN) - window x
                        depth,     // (depth + MARGIN) - window y
                    }),
                )
            } else {
                // No notch: sit below the menu bar as a plain rounded panel.
                unsafe {
                    let _: () = msg_send![ns_win, setLevel: 5isize];
                }
                let area = monitor.work_area();
                let pos = area.position.to_logical::<f64>(scale);
                let size = area.size.to_logical::<f64>(scale);
                let y = pos.y + MARGIN;
                (
                    Layout {
                        x: pos.x + MARGIN,
                        y,
                        width: size.width - MARGIN * 2.0,
                        half: screen.height / 2.0 - y - MARGIN,
                        full: screen.height - y - MARGIN,
                        level: 5,
                    },
                    None,
                )
            };
            *LAYOUT.lock().unwrap() = Some(layout);
            app.manage(Geometry { cutout });

            // Menu bar item: same H icon, toggle + quit.
            let toggle_item = tauri::menu::MenuItemBuilder::with_id("toggle", "Show/Hide Overlay")
                .accelerator("Cmd+Shift+Space")
                .build(app)?;
            let quit_item = tauri::menu::MenuItemBuilder::with_id("quit", "Quit Hypervisor").build(app)?;
            let menu = tauri::menu::MenuBuilder::new(app)
                .items(&[&toggle_item, &quit_item])
                .build()?;
            tauri::tray::TrayIconBuilder::with_id("hv-tray")
                .icon(tauri::image::Image::from_bytes(include_bytes!("../icons/tray.png"))?)
                // Template mode: macOS tints the black glyph to match native
                // menu bar icons (dark on light bars, light on dark bars).
                .icon_as_template(true)
                .menu(&menu)
                .on_menu_event(|app, event| match event.id().as_ref() {
                    "toggle" => toggle_overlay(app),
                    "quit" => app.exit(0),
                    _ => {}
                })
                .build(app)?;

            // Stay hidden; the page calls reveal_ready once it has painted
            // its zero-height mask, and the window first appears then.
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
