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

const MARGIN: f64 = 8.0;
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
    alive: bool,
}

#[derive(serde::Serialize)]
struct Peek {
    session: Session,
    blocks: Vec<String>,
}

fn sessions_dir() -> PathBuf {
    let home = std::env::var("HOME").unwrap_or_else(|_| "/tmp".into());
    PathBuf::from(home).join(".hypervisor").join("sessions")
}

/// Single-quote a string for POSIX shells.
fn shq(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

fn tmux(args: &[&str]) -> Result<String, String> {
    let out = Command::new("tmux")
        .args(args)
        .output()
        .map_err(|e| format!("tmux: {e}"))?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    } else {
        Err(String::from_utf8_lossy(&out.stderr).trim().to_string())
    }
}

fn live_sessions() -> HashSet<String> {
    tmux(&["list-sessions", "-F", "#S"])
        .map(|s| s.lines().map(str::to_string).collect())
        .unwrap_or_default()
}

fn valid_name(name: &str) -> bool {
    !name.is_empty() && name.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
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

    let agent_cmd = match provider.as_str() {
        "anthropic" => format!("claude --model {} {}", shq(&model), shq(&prompt)),
        "openai" => format!("codex --model {} {}", shq(&model), shq(&prompt)),
        _ => format!("opencode --model {} --prompt {}", shq(&model), shq(&prompt)),
    };
    // The tmux server may have been spawned with a bare environment; carry
    // this app's PATH (nvm, homebrew, …) into the session explicitly.
    let path = std::env::var("PATH").unwrap_or_default();
    let cmd = format!("export PATH={}; exec {agent_cmd}", shq(&path));
    let home = std::env::var("HOME").map_err(|e| e.to_string())?;
    // Agent CLIs gate untrusted directories behind an interactive prompt the
    // overlay can't answer, so sessions start in the (trusted) hv repo until
    // there's a real working-directory picker.
    let repo = format!("{home}/git/hv");
    let cwd = if PathBuf::from(&repo).is_dir() { repo } else { home };

    tmux(&["new-session", "-d", "-s", &name, "-c", &cwd, &cmd])?;

    let session = Session {
        name: name.clone(),
        provider,
        model,
        label,
        prompt,
        created_at: created,
        alive: true,
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
    let live = live_sessions();
    let mut sessions: Vec<Session> = fs::read_dir(sessions_dir())
        .map(|rd| {
            rd.filter_map(|entry| {
                let path = entry.ok()?.path();
                if path.extension()? != "json" {
                    return None;
                }
                let mut s: Session = serde_json::from_slice(&fs::read(path).ok()?).ok()?;
                s.alive = live.contains(&s.name);
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
    session.alive = live_sessions().contains(&name);
    let blocks = if session.alive {
        let text = tmux(&["capture-pane", "-p", "-t", &name, "-S", "-400"]).unwrap_or_default();
        parse_blocks(&text)
    } else {
        Vec::new()
    };
    Ok(Peek { session, blocks })
}

#[tauri::command]
fn send_prompt(name: String, text: String) -> Result<(), String> {
    if !valid_name(&name) {
        return Err("invalid session name".into());
    }
    // A newline inside the literal text would submit the TUI input early.
    let text = text.replace(['\n', '\r'], " ").trim().to_string();
    if text.is_empty() {
        return Err("empty prompt".into());
    }
    if !live_sessions().contains(&name) {
        return Err("session is not running".into());
    }
    tmux(&["send-keys", "-t", &name, "-l", "--", &text])?;
    // Let the TUI ingest the literal text before the submit keypress.
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

/// Slice a captured pane into per-exchange blocks (a "> …" prompt line plus
/// the response that follows it) and keep the last three.
fn parse_blocks(text: &str) -> Vec<String> {
    let mut lines: Vec<&str> = text.lines().collect();
    while lines.last().is_some_and(|l| l.trim().is_empty()) {
        lines.pop();
    }
    // Drop the TUI's input box if it's the trailing element of the pane.
    if let Some(idx) = lines.iter().rposition(|l| l.trim_start().starts_with('╭')) {
        if lines.len() - idx <= 6 {
            lines.truncate(idx);
        }
    }
    let cleaned: Vec<&str> = lines
        .into_iter()
        .filter(|l| {
            let t = l.trim();
            if t.is_empty() {
                return true;
            }
            // Horizontal-rule separators between the transcript and input box.
            if t.chars().all(|c| matches!(c, '─' | '━' | '═')) {
                return false;
            }
            !(t.contains("? for shortcuts")
                || t.contains("esc to interrupt")
                || t.contains("shift+tab to cycle")
                || t.contains("-- INSERT --")
                || t.contains("-- NORMAL --")
                || t.starts_with("● "))
        })
        .collect();

    let mut blocks: Vec<Vec<&str>> = Vec::new();
    let mut cur: Vec<&str> = Vec::new();
    for l in cleaned {
        let t = l.trim_start();
        if t.starts_with("> ") || t.starts_with("❯ ") || t.starts_with("› ") {
            if !cur.is_empty() {
                blocks.push(std::mem::take(&mut cur));
            }
        }
        cur.push(l);
    }
    if !cur.is_empty() {
        blocks.push(cur);
    }

    let mut out: Vec<String> = blocks
        .into_iter()
        .map(|b| {
            // An unsegmented pane comes through as one giant block; cap it.
            let tail: Vec<&str> = b.iter().rev().take(40).rev().copied().collect();
            tail.join("\n").trim_matches('\n').to_string()
        })
        .filter(|b| {
            // Drop empty blocks and the TUI's bare input prompt.
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
    use super::parse_blocks;

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

    #[test]
    fn unsegmented_output_returns_tail() {
        let text = (0..100).map(|i| format!("line {i}\n")).collect::<String>();
        let blocks = parse_blocks(&text);
        assert_eq!(blocks.len(), 1);
        assert!(blocks[0].contains("line 99"));
        assert!(!blocks[0].contains("line 10\n"), "should cap to a tail");
    }
}

// ---------------------------------------------------------------------------

fn main() {
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
            kill_session
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
