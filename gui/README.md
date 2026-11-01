# tailmix GUI

A libadwaita front end for the tailmix daemon. It is a supplement to the
`tailmix` and `tailmixd` command line tools, not a replacement: both read and
write the same state, and the GUI never talks to a tailnet directly.

## How it works

The GUI has no privileged components of its own.

- Reads go straight to the daemon's unix socket (`/var/run/tailmix/tailmixd.sock`,
  or `$TAILMIX_SOCKET_DIR`) with plain HTTP over the socket.
- Policy changes run the installed `tailmix` binary through `pkexec`, so every
  change goes through the same CLI code path and the same polkit prompt a
  terminal user sees.
- The daemon is polled every two seconds while a window is visible.

## Build

Requires Rust 1.83 or newer, GTK 4.10 or newer, and libadwaita 1.6 or newer with
their development headers.

```sh
make gui            # cargo build --release --manifest-path gui/Cargo.toml
sudo make install-gui
```

`make install-gui` installs the binary, the desktop entry, the app icon, and the
AppStream metadata under `PREFIX` (default `/usr/local`).

## Tests

```sh
cargo test --manifest-path gui/Cargo.toml
```

Nothing in this directory is part of the Go module, so `go build ./...` and the
release archive do not include the GUI.
