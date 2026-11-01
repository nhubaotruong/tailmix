//! Everything the GUI does to tailmix, in one place.
//!
//! Reads use the daemon's unix socket directly: it is world-readable and the
//! daemon allows `GET` from any user. Every change shells out to the installed
//! `tailmix` binary through `pkexec`, because the daemon rejects mutations from
//! non-root peers (cmd/tailmixd/controlserver.go:82-98) and the CLI already owns
//! argument parsing, consent prompts, and error messages.

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;

use gtk::glib;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Duration;

use serde::de::DeserializeOwned;

const SOCKET_NAME: &str = "tailmixd.sock";
const READ_TIMEOUT: Duration = Duration::from_secs(5);
/// Absolute, so a planted `pkexec` earlier on `$PATH` cannot answer instead.
const PKEXEC: &str = "/usr/bin/pkexec";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FailureKind {
    /// The daemon is not running, or its socket is unreachable.
    Unavailable,
    /// Authorisation was dismissed or refused.
    Denied,
    /// Anything the CLI or the daemon reported as an error.
    Failed,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Failure {
    pub kind: FailureKind,
    pub code: String,
    pub message: String,
}

impl Failure {
    fn new(kind: FailureKind, code: &str, message: impl Into<String>) -> Self {
        Failure {
            kind,
            code: code.to_string(),
            message: message.into(),
        }
    }

    /// A background task that panicked or was cancelled.
    pub fn internal(message: &str) -> Self {
        Failure::new(FailureKind::Failed, "internal", message)
    }
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
struct ErrorRecord {
    code: String,
    message: String,
}

/// Resolved locations of the tailmix pieces the GUI drives.
#[derive(Debug, Clone)]
pub struct Bridge {
    pub tailmix: Option<PathBuf>,
    pub socket_dir: PathBuf,
}

impl Bridge {
    pub fn discover() -> Self {
        // The same binary is run as root, so only a root-owned, unwritable copy
        // is acceptable; anything else could be replaced by the desktop user.
        let tailmix = std::iter::once(glib::find_program_in_path("tailmix"))
            .flatten()
            .chain(["/usr/local/bin/tailmix", "/usr/bin/tailmix"].map(PathBuf::from))
            .find(|path| trusted(path));
        let socket_dir = std::env::var_os("TAILMIX_SOCKET_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("/var/run/tailmix"));
        Bridge {
            tailmix,
            socket_dir,
        }
    }

    pub fn socket_path(&self) -> PathBuf {
        self.socket_dir.join(SOCKET_NAME)
    }

    /// Reads one daemon endpoint. Runs off the main loop; caller owns threading.
    pub fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T, Failure> {
        let body = self.get_raw(path)?;
        serde_json::from_str(&body).map_err(|err| {
            Failure::new(
                FailureKind::Failed,
                "internal",
                format!("unexpected response from tailmixd: {err}"),
            )
        })
    }

    fn get_raw(&self, path: &str) -> Result<String, Failure> {
        let socket = self.socket_path();
        let mut stream = UnixStream::connect(&socket).map_err(|err| {
            Failure::new(
                FailureKind::Unavailable,
                "daemon_unavailable",
                format!("tailmixd is not reachable at {}: {err}", socket.display()),
            )
        })?;
        stream.set_read_timeout(Some(READ_TIMEOUT)).ok();
        stream.set_write_timeout(Some(READ_TIMEOUT)).ok();
        // HTTP/1.0 keeps the daemon from answering with chunked framing, which
        // this minimal client does not want to reassemble.
        let request = format!("GET {path} HTTP/1.0\r\nHost: tailmix\r\nConnection: close\r\n\r\n");
        stream.write_all(request.as_bytes()).map_err(|err| {
            Failure::new(
                FailureKind::Unavailable,
                "daemon_unavailable",
                format!("cannot write to tailmixd: {err}"),
            )
        })?;
        let mut response = Vec::new();
        stream.read_to_end(&mut response).map_err(|err| {
            Failure::new(
                FailureKind::Unavailable,
                "daemon_unavailable",
                format!("cannot read from tailmixd: {err}"),
            )
        })?;
        let response = String::from_utf8_lossy(&response).into_owned();
        let (head, body) = response.split_once("\r\n\r\n").unwrap_or((&response, ""));
        let status = head
            .lines()
            .next()
            .and_then(|line| line.split_whitespace().nth(1))
            .and_then(|code| code.parse::<u16>().ok())
            .unwrap_or(0);
        let body = if head.lines().any(is_chunked) {
            dechunk(body)
        } else {
            body.to_string()
        };
        if status == 200 {
            return Ok(body);
        }
        let record: ErrorRecord = serde_json::from_str(&body).unwrap_or_default();
        Err(Failure::new(
            kind_for_status(status),
            if record.code.is_empty() {
                "internal"
            } else {
                &record.code
            },
            if record.message.is_empty() {
                format!("tailmixd returned HTTP {status}")
            } else {
                record.message
            },
        ))
    }

    /// The `tailmix` binary this app drives.
    fn command(&self) -> Result<PathBuf, Failure> {
        self.tailmix.clone().ok_or_else(|| {
            Failure::new(
                FailureKind::Failed,
                "internal",
                "tailmix is not installed by root; install it under /usr/local/bin or /usr/bin",
            )
        })
    }

    /// Starts the daemon unit. `tailmix up` needs a live daemon, so the
    /// recovery path goes through systemd instead.
    pub fn start_daemon(&self) -> Result<String, Failure> {
        let output = Command::new(PKEXEC)
            .arg("--disable-internal-agent")
            .args(["systemctl", "start", "tailmixd"])
            .stdin(Stdio::null())
            .output()
            .map_err(|err| {
                Failure::new(
                    FailureKind::Failed,
                    "internal",
                    format!("cannot run pkexec: {err}"),
                )
            })?;
        if output.status.success() {
            return Ok(String::new());
        }
        let message = String::from_utf8_lossy(&output.stderr).trim().to_string();
        let kind = match output.status.code() {
            Some(126) => FailureKind::Denied,
            _ => FailureKind::Failed,
        };
        Err(Failure::new(
            kind,
            "internal",
            if message.is_empty() {
                "the daemon did not start".to_string()
            } else {
                message
            },
        ))
    }

    /// Runs a mutating `tailmix` command under `pkexec`.
    ///
    /// `stdin` carries secrets; it is the only channel pkexec leaves open.
    pub fn mutate(&self, args: &[String], stdin: Option<&str>) -> Result<String, Failure> {
        let tailmix = self.command()?;
        let mut command = Command::new(PKEXEC);
        command
            .arg("--disable-internal-agent")
            .arg(&tailmix)
            .args(args)
            .arg("--json")
            .stdin(if stdin.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let mut child = command.spawn().map_err(|err| {
            Failure::new(
                FailureKind::Failed,
                "internal",
                format!("cannot run pkexec: {err}"),
            )
        })?;
        if let Some(secret) = stdin {
            if let Some(mut pipe) = child.stdin.take() {
                pipe.write_all(secret.as_bytes()).ok();
            }
        }
        let output = child.wait_with_output().map_err(|err| {
            Failure::new(
                FailureKind::Failed,
                "internal",
                format!("cannot wait for tailmix: {err}"),
            )
        })?;
        let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
        let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
        if output.status.success() {
            return Ok(stdout);
        }
        Err(failure_from_stderr(output.status.code(), &stderr))
    }

    /// Runs `tailmix <args>` unchanged, for the Tailscale passthrough where
    /// every argument belongs to the upstream command.
    pub fn run_raw(&self, args: &[String]) -> Result<String, Failure> {
        let tailmix = self.command()?;
        let output = Command::new(&tailmix)
            .args(args)
            .stdin(Stdio::null())
            .output()
            .map_err(|err| {
                Failure::new(
                    FailureKind::Unavailable,
                    "internal",
                    format!("cannot run {}: {err}", tailmix.display()),
                )
            })?;
        let mut text = String::from_utf8_lossy(&output.stdout).into_owned();
        let errors = String::from_utf8_lossy(&output.stderr);
        if !errors.trim().is_empty() {
            if !text.is_empty() {
                text.push('\n');
            }
            text.push_str(errors.trim());
        }
        Ok(text)
    }

    /// Runs `tailmix <args> --json` as the current user, for reads the daemon
    /// socket cannot answer.
    pub fn read_cli(&self, args: &[String]) -> Result<String, Failure> {
        let tailmix = self.command()?;
        let output = Command::new(&tailmix)
            .args(args)
            .arg("--json")
            .stdin(Stdio::null())
            .output()
            .map_err(|err| {
                Failure::new(
                    FailureKind::Unavailable,
                    "internal",
                    format!("cannot run {}: {err}", tailmix.display()),
                )
            })?;
        if output.status.success() {
            return Ok(String::from_utf8_lossy(&output.stdout).into_owned());
        }
        let stderr = String::from_utf8_lossy(&output.stderr);
        Err(failure_from_stderr(None, &stderr))
    }
}

fn kind_with_code(kind: FailureKind, code: &str) -> FailureKind {
    match code {
        "permission_denied" => FailureKind::Denied,
        "daemon_unavailable" => FailureKind::Unavailable,
        _ => kind,
    }
}

fn kind_for_status(status: u16) -> FailureKind {
    match status {
        401 | 403 => FailureKind::Denied,
        503 => FailureKind::Unavailable,
        _ => FailureKind::Failed,
    }
}

fn last_line(text: &str) -> Option<&str> {
    text.lines().map(str::trim).rfind(|line| !line.is_empty())
}

/// The trailing JSON error record a `--json` command writes to stderr.
///
/// The record is indented, so it spans lines; try each `{` from the end.
fn error_record(stderr: &str) -> Option<ErrorRecord> {
    let text = stderr.trim_end();
    text.match_indices('{')
        .rev()
        .find_map(|(index, _)| serde_json::from_str(&text[index..]).ok())
}

fn is_chunked(line: &str) -> bool {
    let lower = line.to_ascii_lowercase();
    lower.starts_with("transfer-encoding:") && lower.contains("chunked")
}

/// Reassembles a chunked body without pulling in an HTTP client.
fn dechunk(body: &str) -> String {
    let mut rest = body;
    let mut text = String::new();
    while let Some((size_line, tail)) = rest.split_once("\r\n") {
        let size = usize::from_str_radix(size_line.split(';').next().unwrap_or("").trim(), 16);
        let Ok(size) = size else { break };
        if size == 0 || tail.len() < size {
            text.push_str(&tail[..tail.len().min(size)]);
            break;
        }
        text.push_str(&tail[..size]);
        rest = tail[size..].trim_start_matches("\r\n");
    }
    text
}

/// Root owns the binary and nobody else can write it.
fn trusted(path: &Path) -> bool {
    use std::os::unix::fs::MetadataExt;
    std::fs::metadata(path)
        .map(|meta| meta.uid() == 0 && meta.mode() & 0o022 == 0)
        .unwrap_or(false)
}

/// Maps a failed `pkexec tailmix … --json` run to the message the app shows.
fn failure_from_stderr(code: Option<i32>, stderr: &str) -> Failure {
    let record = error_record(stderr);
    let (kind, fallback) = match code {
        Some(126) => (
            FailureKind::Denied,
            "Authentication was dismissed or refused.",
        ),
        Some(127) => (
            FailureKind::Failed,
            "Polkit could not authorise the change.",
        ),
        _ => (FailureKind::Failed, "tailmix reported an error."),
    };
    match record {
        Some(record) if !record.message.is_empty() => Failure::new(
            kind_with_code(kind, &record.code),
            &record.code,
            record.message,
        ),
        _ => {
            let message = match code {
                // pkexec's own failures carry no tailmix JSON record.
                Some(126) | Some(127) => fallback,
                _ => last_line(stderr).unwrap_or(fallback),
            };
            Failure::new(kind, "internal", message)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_the_json_error_record() {
        let stderr =
            "warning: something\n{\"code\":\"permission_denied\",\"message\":\"need root\"}\n";
        let failure = failure_from_stderr(Some(1), stderr);
        assert_eq!(failure.kind, FailureKind::Denied);
        assert_eq!(failure.code, "permission_denied");
        assert_eq!(failure.message, "need root");
    }

    #[test]
    fn a_dismissed_prompt_is_a_denial() {
        let failure = failure_from_stderr(Some(126), "Error executing command as another user");
        assert_eq!(failure.kind, FailureKind::Denied);
        assert_eq!(failure.message, "Authentication was dismissed or refused.");
    }

    #[test]
    fn a_missing_agent_names_polkit() {
        let failure = failure_from_stderr(Some(127), "Error executing command as another user");
        assert_eq!(failure.kind, FailureKind::Failed);
        assert_eq!(failure.message, "Polkit could not authorise the change.");
    }

    #[test]
    fn an_unreachable_daemon_is_not_a_denial() {
        let failure = failure_from_stderr(
            Some(1),
            "{\"code\":\"daemon_unavailable\",\"message\":\"it is down\"}",
        );
        assert_eq!(failure.kind, FailureKind::Unavailable);
    }
}
