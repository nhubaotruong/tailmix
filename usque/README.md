# tailmix usque fork

This package was forked from
[`Dinboy1123/usque@6aa03fc97d12848dce34eedbd187fb1077b5d1ea`](https://github.com/Diniboy1123/usque/commit/6aa03fc97d12848dce34eedbd187fb1077b5d1ea),
tag `v4.2.1`. The machine-readable source record is in [UPSTREAM](UPSTREAM).

usque is a Go implementation of the Cloudflare WARP client in MASQUE mode
(RFC 9484 Connect-IP). tailmix carries it as a profile engine, so a host can
route traffic to Cloudflare WARP beside its tailnet routes.

The fork exists because upstream is a CLI: its tunnel loop returns nothing, it
reports connection state only through executable hooks, it exits the process
when a configuration does not match the connection mode, and it logs the
endpoint it dials. An embedded engine needs the opposite of each.

The copied files retain their upstream copyright headers and are distributed
under the accompanying MIT license.

## Behavioral differences

The tree is no longer a standalone program. It is a library inside the tailmix
module, at `github.com/maisem/tailmix/usque`.

- **No nested module, no CLI.** `go.mod`, `go.sum`, `main.go`, `cmd/`,
  `dns_android.go`, the Dockerfile, and the goreleaser configuration are gone,
  together with the CLI-only helpers under `internal/` and `config/`. What
  remains is `api/` (tunnel, MASQUE, Cloudflare API), `config/` (config file
  shape), `internal/` (constants and key helpers), and `models/`.
- **State through a Go callback.** `MaintainTunnelConfig.OnStateChange` receives
  `TunnelConnecting`, `TunnelConnected`, or `TunnelDisconnected`. The
  subprocess hook runner and its environment (`USQUE_ENDPOINT` included) are
  removed.
- **Errors instead of process exit.** `MaintainTunnel` returns an error, and a
  configuration that upstream would have rejected with `log.Fatalf` returns an
  error from it instead: no device, no MTU, no TLS config, or an endpoint whose
  type does not match the connection mode. It returns the context error when
  the context is cancelled.
- **Caller-supplied transport.** `Transport` carries `ListenPacket` for the
  QUIC path and `DialContext` for the HTTP/2 path. tailmix fills both from its
  network namespace sockets, so the tunnel leaves through the host's physical
  link. The HTTP/2 proxy environment variables upstream honoured are not read:
  a proxy dial would bypass the caller's transport. The twenty-byte QUIC
  connection ID, the HTTP/3 datagram setting, and the retry of a
  `PROTOCOL_VIOLATION` connect are kept.
- **No endpoint in logs or errors.** The connection log names no address, and
  error text is redacted before it reaches the caller's log function: the
  endpoint, its host, and the Connect and Access host names become
  `<endpoint>`. `MaintainTunnelConfig.Logf` is the only log sink.
- **Bounded exponential backoff.** A failed connect, a rejected connect, and a
  lost connection each wait one second, doubling to a thirty-second cap, with
  twenty percent jitter. The attempt counter is not reset while the call runs,
  so a tunnel that keeps dropping converges on the cap. Upstream used a fixed
  delay.
- **Always reconnects, and reads the device only after connecting.**
  `AlwaysReconnect` and `ReconnectDelay` are gone, and the pre-connect device
  read is removed: the device is read once the connection exists, so the first
  packet after a drop is not consumed while the tunnel is down.
- **Device adapter matches tailmix's `tun.Device`.** `DeviceAdapter` implements
  the batch `Read(slab []byte, packets []tun.ReadPacket)` shape used by
  `tunmux.ChanTUN`, with a compile-time interface assertion. The
  `songgao/water` adapter and its dependency are removed.
- **Removed CLI-only features.** The L4 proxy, SOCKS and HTTP proxy helpers,
  port mapping, endpoint selection from a config file, and the Android DNS
  helper are not part of the library.

`api/tunnel_test.go` covers the differences that carry behaviour: cancellation,
endpoint-free logging, caller transport use, device round trip, backoff growth,
retry after a lost connection, and configuration errors.
