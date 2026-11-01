# tailmix-gui

A GNOME app for tailmix, built with Rust, GTK 4, and libadwaita.

It is a supplement to the `tailmix` command line, not a replacement. Both drive
the same `tailmixd` daemon and read the same state, so a change made in either
place shows up in the other on the next refresh.

The crate lives in `gui/` for now and moves to its own repository later.

## How it talks to tailmix

- Reads go straight to the daemon's unix socket (`$TAILMIX_SOCKET_DIR` or
  `/var/run/tailmix/tailmixd.sock`), which allows `GET` from any user.
- Every change runs the installed `tailmix` binary through
  `pkexec --disable-internal-agent`, because the daemon rejects mutations from
  non-root peers. Polkit asks for authorisation once per change.
- No new privileged surface is added: the app never runs as root and never
  writes outside the user's session.

## Build

Needs `gtk4-devel`, `libadwaita-devel` (1.6 or newer), and a Rust toolchain
(1.83 or newer). On an immutable Fedora host, build inside a container:

```sh
podman run -d --name tailmix-gui-build -v "$PWD:/src:Z" -w /src \
  registry.fedoraproject.org/fedora:45 sleep infinity
podman exec tailmix-gui-build dnf install -y gcc pkg-config gtk4-devel \
  libadwaita-devel cargo rust clippy rustfmt
podman exec -w /src/gui tailmix-gui-build cargo build
```

## Run

```sh
./target/debug/tailmix-gui
```

Start it as your desktop user. It refuses to run as root.

## Check

```sh
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
```

## Install

`make install-gui` copies the binary, the desktop entry, the metainfo file, and
the icon into `$(PREFIX)`.
