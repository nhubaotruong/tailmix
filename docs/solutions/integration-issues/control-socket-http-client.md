---
title: Reading the tailmixd control socket from a non-Go client
date: 2026-11-01
category: integration-issues
module: controlapi
problem_type: integration_issue
component: api_layer
symptoms:
  - "A client that is not Go decodes an empty profile list as a missing field"
  - "Reads of a large status body return chunk framing instead of JSON"
  - "Every CLI failure surfaces as a bare closing brace"
  - "A non-Go client cannot find the tailmix binary under pkexec"
root_cause: config_error
resolution_type: code_fix
severity: high
tags: [controlapi, unix-socket, json, pkexec, chunked-encoding]
---

# Reading the tailmixd control socket from a non-Go client

## Problem
`controlapi` is a Go package, so a client in another language talks to `tailmixd` over the unix socket with a hand-written HTTP request and its own JSON decoder. Four properties of the daemon side break that client even though a Go client never notices them.

## Symptoms
- A default install shows "tailmixd is not reachable" while the daemon answers every request.
- A status body around 2 KB or larger fails to parse, so the profile list appears empty.
- A failed `tailmix` command makes the client report only `}` as the error.
- A change that runs as root fails with "the program is not installed", although `tailmix` is on the user's `PATH`.

## What Didn't Work
- Adding `#[serde(default)]` to the models: it fills a missing key, not a key that is present with the value `null`.
- Reading the body until the socket closes and parsing it as is: the daemon frames large bodies with chunk sizes when the client asks for HTTP/1.1.
- Parsing the stderr record line by line: the record is indented across several lines.

## Solution
Four small client-side rules. `gui/src/models.rs`, `gui/src/bridge.rs` in this repo apply them.

1. Decode every slice through an option. Go marshals a nil slice as `null`, so a decoder must accept `null` as the zero value:

```rust
fn null_default<'de, D, T>(deserializer: D) -> Result<T, D::Error>
where D: serde::Deserializer<'de>, T: serde::Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}
```

2. Ask for protocol version 1.0, then dechunk if the header says so. The daemon writes with `json.NewEncoder(w).Encode` (`controlapi/server.go:362`) and never sets `Content-Length`, so Go switches to chunked framing once the body passes its 2048-byte buffer. Requesting version 1.0 makes the server close the connection instead, which delimits the body:

```rust
let version = "1.0";
let request = format!("GET {path} HTTP/{version}\r\nHost: tailmix\r\nConnection: close\r\n\r\n");
```

Keep the dechunk branch as a guard for any future version that answers HTTP/1.1.

3. Parse the trailing JSON object, not the last line. `cmd/tailmix/main.go:1621-1623` sets `SetIndent("", "  ")`, so an error record spans several lines. Scan for `{` from the end of the output and parse from there:

```rust
text.match_indices('{').rev().find_map(|(index, _)| serde_json::from_str(&text[index..]).ok())
```

4. Resolve the binary and `pkexec` to absolute, root-owned paths. `pkexec` clears the environment and resets `PATH`, and a binary found on the user's `PATH` would then run as root. Install paths are `/usr/local/bin/tailmix` or `/usr/bin/tailmix`, and the agent must be the external one because the internal text agent needs `/dev/tty`:

```
/usr/bin/pkexec --disable-internal-agent /usr/local/bin/tailmix <args> --json
```

## Why This Works
Each rule matches a byte-level property of the daemon or the CLI rather than a guess about it. The nil-slice rule covers `null`; requesting protocol version 1.0 removes framing the client would have to parse; the reverse scan finds a record that starts on an earlier line; the absolute path removes both the `PATH` reset and the user-writable-binary risk.

Two consequences worth knowing. `TAILMIX_SOCKET_DIR` reaches reads but never mutations, because `pkexec` clears the environment, so a socket-redirected test proves nothing about a privileged change. And the generic `org.freedesktop.policykit.exec` action is `auth_admin` without `_keep`, so each mutation prompts unless a dedicated policy file ships.

## Prevention
- Decode one real daemon response in a test, with a body over 2 KB and a `null` list, instead of a hand-written fixture.
- Keep the client's HTTP request line explicit (protocol version 1.0, `Connection: close`) and comment why.
- Resolve every privileged executable to an absolute path owned by root.

## Related Issues
- `docs/profile-management.md` documents the socket directory and its permissions.
- `docs/plans/2026-11-01-1426-feat-tailmix-gui-plan.md` records KTD8 and R44-R48 for the same constraint.
