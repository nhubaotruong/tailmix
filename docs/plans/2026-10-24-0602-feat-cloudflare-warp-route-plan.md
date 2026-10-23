---
title: Cloudflare WARP Route Profile - Plan
type: feat
date: 2026-10-24T06:02:49+07:00
topic: cloudflare-warp-route
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-brainstorm
execution: code
---

# Cloudflare WARP Route Profile - Plan

## Goal Capsule

**Objective.** One host carries traffic to its tailnets and to the internet through Cloudflare WARP at the same time, with one command deciding which path a packet takes and one view reporting whether each path is up.

**Means.** A fourth profile kind built on a forked usque.

**Product authority.** `docs/architecture.md` keeps the existing rules: one engine per profile, one shared host TUN, and no active-tailnet switch. This work adds a kind and adds no routing concept.

**Open blockers.** None.

---

## Product Contract

### Summary

A WARP profile runs a MASQUE tunnel through Cloudflare, advertises a full-tunnel default, and takes part in exit-node selection, longest-prefix routing, and DNS exactly as a tailnet exit node does. It needs no peers, no Tailscale identity, and no separate client process.

### Problem Frame

A tailnet exit node sends everything to a network you own. Cloudflare WARP sends everything somewhere else. A host that needs both today runs the usque CLI beside tailmix, and nothing coordinates the two: both claim a default route, two programs write the resolver set, and the operator arranges policy routing by hand. When a lookup or a connection leaves the wrong way, no single place explains which path it took or why.

### Requirements

**Profile kind**

- R1. WARP is a profile kind named `warp`, stored in `state.json` beside tailscale and wireguard profiles and listed by every command that lists profiles.
- R2. `tailmix warp register` creates a WARP profile, registers it with Cloudflare, and starts it in one command.
- R3. `tailmix warp register --jwt <team-token>` enrolls through ZeroTrust instead of a consumer registration.
- R4. `tailmix warp register --config <path>` creates the profile from a `config.json` written by the usque CLI, without contacting Cloudflare.
- R5. A WARP profile is enabled, disabled, renamed, aliased and removed through the existing profile commands, with no WARP-specific variant.
- R6. Removing a WARP profile deletes its stored credentials and its state directory.

**Credentials**

- R7. A WARP profile stores its private key, registration ID, access token, license, endpoint selection, and the tunnel addresses Cloudflare assigned, inside the profile's own state directory.
- R8. The private key and access token files are readable only by their owner.
- R9. No command prints a private key, an access token, or a license; profile output reports only whether usable credentials exist.
- R10. A WARP profile without usable credentials reports a distinct backend state that names re-registration as the fix, and opens no tunnel.
- R11. Re-registration replaces stored credentials atomically, so an interrupted re-key leaves the previous credentials in place.

**Tunnel engine**

- R12. The engine opens a MASQUE Connect-IP tunnel to the registered endpoint, over QUIC by default, with a per-profile option to use the HTTP/2 path.
- R13. The engine reports the tunnel addresses assigned at registration as its self addresses, so route policy can compute its host sources.
- R14. The engine reports connection-state changes through the update channel the existing engines use, so the supervisor reconciles without polling.
- R15. The engine reconnects after a dropped tunnel with backoff and reports each transition, rather than exiting or blocking.
- R16. `Close` stops the tunnel and releases the device, and no goroutine survives it.
- R17. The engine exposes no exit-node preference control, no route-acceptance control, and no local Tailscale backend, and the supervisor treats each absence as normal for this kind.
- R18. Packets the engine reads from its device leave through the tunnel, and packets the tunnel delivers are written back to the device, with no address translation inside the engine.
- R19. A stalled handshake surfaces as an unavailable backend state and never blocks the reconcile loop.

**Routing**

- R20. A WARP profile advertises `0.0.0.0/0` and `::/0` as its available routes.
- R21. Selecting a WARP profile as exit node gives it the default route for each address family it has a source for.
- R22. An explicit `routes bind` for a more specific prefix still wins over the WARP default.
- R23. A WARP exit-node selection survives a restart, and validation of the stored selection is kind-aware.
- R24. Only one profile holds the default route: selecting a WARP exit node clears a tailnet exit-node selection, and the reverse.
- R25. A WARP profile that is not the selected exit node carries only the traffic an explicit binding sends to it.

**DNS**

- R26. While a WARP profile is the selected exit node, it contributes a configurable resolver address as the root `.` DNS route, as an exit profile does.
- R27. A MagicDNS suffix from any tailnet still wins over the WARP root route by longest suffix.
- R28. Unselecting WARP removes its DNS route, and a WARP profile that is not the exit node contributes no DNS route.

**CLI and status**

- R29. `tailmix profile list` and `tailmix status` show a WARP profile with its kind, backend state, and whether it holds the default route.
- R30. `tailmix status` for a WARP profile shows its tunnel address, the time since its last successful connection, and an empty peer list.
- R31. `tailmix exit-node` accepts a WARP profile name with no peer argument and lists the WARP profile among the available exit nodes.
- R32. WARP errors use the existing error codes, plus one code that separates a credential or registration problem from an unavailable tunnel.
- R33. `tailmix warp register` writes no profile when Cloudflare rejects the request or the network is unreachable.
- R34. Completion for the new subcommand and its flags comes from the existing completion path.

**Platform and safety**

- R35. Creating a WARP profile on a platform other than Linux fails with a clear error that names the platform limit.
- R36. A state file that names a WARP profile still loads on macOS, that profile reports the platform error, and the other profiles keep working.
- R37. The forked source lands as one unmodified snapshot commit of the pinned upstream revision, with upstream copyright headers, its license, a README that links the upstream repository and commit, and an `UPSTREAM` record.
- R38. tailmix adaptations to the fork land in commits after that snapshot.
- R39. The fork removes fatal exits and endpoint logging from the tunnel path.
- R40. No new log line names an IP address, a DNS name, a node name, a peer identity, or a packet endpoint; registration and connection logs name the profile only.
- R41. The generated license report is regenerated in the same change, and the license check passes.

### Key Decisions

- **WARP behaves as a peerless exit node.** It advertises a full-tunnel default and reuses the existing default-route, longest-prefix, and exit-node-selection rules, so route policy gains no new class. Governs R20, R21, R22, R23, R24, R31. (session-settled: user-directed — chosen over a new route class: no new policy surface to learn or fail closed on)
- **Support both registration and config import.** The Cloudflare registration API is called directly, and a usque `config.json` can be imported when one already exists. Governs R2, R3, R4, R7. (session-settled: user-directed — chosen over registration only: nobody must keep usque installed to migrate)
- **WARP DNS behaves as an exit profile's DNS.** The root `.` route appears only while WARP is the selected exit node, and tailnet suffixes keep priority by longest match. Governs R26, R27, R28. (session-settled: user-directed)
- **Linux only for this change.** The engine, the state-file tolerance, and the error path are scoped to Linux and to failing loudly elsewhere. Governs R35, R36. (session-settled: user-directed — chosen over macOS parity now: it splits the test surface)
- **Fork usque rather than consume its `api` package.** Upstream `MaintainTunnel` returns nothing and reports connect state only through executable-path hooks, so a status channel and Go callbacks are unavoidable. Governs R12, R14, R15, R37, R38, R39. (session-settled: user-directed — chosen over shelling out to the usque binary: status, reconnect, and privacy control stay inside the daemon)

### Actors

- A1. Operator. Runs `tailmix warp register`, selects an exit node, binds prefixes, and reads status.
- A2. Daemon. Owns desired state, constructs one engine per profile, and reconciles the route table, host routes, and DNS.
- A3. WARP engine. Holds the tunnel, reports state, and moves packets between its device and Cloudflare.
- A4. Cloudflare registration API. Issues credentials and tunnel addresses for a device registration.

### Key Flows

- F1. First registration.
  - **Trigger:** The operator runs `tailmix warp register`.
  - **Actors:** A1, A2, A4.
  - **Steps:** The daemon requests registration, receives credentials and tunnel addresses, writes the profile, and starts the engine.
  - **Outcome:** The tunnel is up and `tailmix status` reports the profile connected.
  - **Covered by:** R2, R7, R13.
- F2. Unattended start.
  - **Trigger:** The daemon starts with stored WARP credentials.
  - **Actors:** A2, A3.
  - **Steps:** The engine dials, reports state transitions, and settles in the running state.
  - **Outcome:** The profile is connected with no user action.
  - **Covered by:** R14, R15, R19.
- F3. Default switch to WARP.
  - **Trigger:** The operator runs `tailmix exit-node` with the WARP profile and no peer.
  - **Actors:** A1, A2, A3.
  - **Steps:** A tailnet exit-node selection is cleared, WARP gains the default for both families, and the root DNS route points at the WARP resolver.
  - **Outcome:** Unbound traffic leaves through WARP.
  - **Covered by:** R21, R24, R26, R31.
- F4. Default switch back.
  - **Trigger:** The operator selects a tailnet peer as exit node.
  - **Actors:** A1, A2.
  - **Steps:** WARP loses the default and its DNS route, and the tailnet exit node takes both.
  - **Outcome:** Unbound traffic returns to the tailnet and the resolver set returns to its previous state.
  - **Covered by:** R24, R28.
- F5. Prefix override while WARP is default.
  - **Trigger:** The operator binds `10.0.0.0/8` to a tailnet while WARP holds the default.
  - **Actors:** A1, A2.
  - **Steps:** The longer prefix wins for that range and every other destination keeps the WARP default.
  - **Outcome:** Both paths carry traffic at once.
  - **Covered by:** R22, R25.
- F6. WARP alongside a tailnet default.
  - **Trigger:** A tailnet exit node holds the default while the WARP profile is enabled but unselected.
  - **Actors:** A2, A3.
  - **Steps:** WARP keeps its tunnel up and receives only bound prefixes.
  - **Outcome:** The tailnet default is unchanged.
  - **Covered by:** R25.
- F7. Import.
  - **Trigger:** The operator runs `tailmix warp register --config <path>`.
  - **Actors:** A1, A2.
  - **Steps:** The profile is written from the file without a Cloudflare call, and the engine starts.
  - **Outcome:** An existing usque registration works without re-registering.
  - **Covered by:** R4.
- F8. Restart.
  - **Trigger:** The daemon restarts while WARP is the selected exit node.
  - **Actors:** A2, A3.
  - **Steps:** The selection loads intact, the engine reconnects, and the default is rebuilt.
  - **Outcome:** The host returns to its previous routing state.
  - **Covered by:** R15, R23.
- F9. Unsupported host.
  - **Trigger:** The operator runs `tailmix warp register` on macOS.
  - **Actors:** A1, A2.
  - **Steps:** The command fails with the platform error and writes no profile.
  - **Outcome:** The state file is unchanged.
  - **Covered by:** R35.

### Acceptance Examples

- AE1.
  - **Covers R23.**
  - **Given:** A state file holds an exit-node selection with an empty node ID and an invalid peer IP against a `warp` profile.
  - **When:** The daemon loads the file.
  - **Then:** The selection is still present.
- AE2.
  - **Covers R24.**
  - **Given:** A tailnet exit node is selected.
  - **When:** The operator selects WARP, then later selects the tailnet peer again.
  - **Then:** Each selection clears the other, and both the default route and the root DNS route move with it.
- AE3.
  - **Covers R27.**
  - **Given:** A tailnet advertises `corp.example` and WARP is the exit node.
  - **When:** The host resolves `host.corp.example` and then `example.com`.
  - **Then:** The first lookup reaches the tailnet resolver and the second reaches the WARP resolver.
- AE4.
  - **Covers R21.**
  - **Given:** WARP is the exit node and holds a source for both families.
  - **When:** The host sends to a public IPv4 address and to a public IPv6 address.
  - **Then:** Both leave through the WARP tunnel.
- AE5.
  - **Covers R22.**
  - **Given:** WARP holds the default and `10.0.0.0/8` is bound to a tailnet.
  - **When:** The host sends to `10.1.2.3` and then to a public address.
  - **Then:** The first reaches the tailnet and the second leaves through WARP.
- AE6.
  - **Covers R10.**
  - **Given:** The credential file of a running WARP profile is deleted.
  - **When:** The daemon reconciles.
  - **Then:** The profile reports the re-registration state, opens no tunnel, and the tailnet profiles stay running.
- AE7.
  - **Covers R36.**
  - **Given:** A state file that names a WARP profile is opened on macOS.
  - **When:** The daemon loads it.
  - **Then:** The WARP profile reports the platform error and the tailnet profiles still connect.
- AE8.
  - **Covers R15.**
  - **Given:** A WARP tunnel drops after a period of idleness.
  - **When:** The host sends new traffic.
  - **Then:** The profile reports a reconnect transition and returns to running without a daemon restart or a command.

### Success Criteria

- One command takes a host from no WARP state to a profile that `tailmix status` reports as connected, with no usque binary installed.
- A host with a tailnet exit node and a WARP profile moves the default and the root DNS route between them with one command each, in one direction and back, without a restart.
- A bound prefix reaches its tailnet while WARP holds the default, and the reverse.
- A state file with no WARP profile produces the same route table, host routes, and DNS configuration as before this change.
- `make check` passes, and the license check passes with the fork's license recorded.

### Scope Boundaries

- Out of scope: WARP-to-WARP port forwarding, the `socks`, `http-proxy`, and `l4-socks` modes, per-app split tunneling inside WARP, NAT64 and IPv6-only operation, and WARP in WireGuard mode.
- Out of scope: failover between WARP and a tailnet exit node when one becomes unavailable.
- Out of scope: macOS and Windows support, and any change to the existing SOCKS mode.
- Out of scope: a graphical surface, and any new route-policy class.

### Dependencies and Assumptions

- The fork base is usque tag `v4.2.1`, commit `6aa03fc97d12848dce34eedbd187fb1077b5d1ea`, which is the latest tagged revision.
- tailmix already vendors `gvisor` and `tailscale/wireguard-go`, so the fork's device import repoints to those copies without a new module.
- Cloudflare's MASQUE endpoint is reachable on UDP 443, or on TCP 443 for the HTTP/2 path.
- Cloudflare's registration API keeps its current shape; a change there affects R2 and R3 only.
- The WARP tunnel address family and address values come from registration, not from tailmix's own pools.

### Outstanding Questions

**Resolve Before Planning**

None.

**Deferred to Planning**

- The device MTU, given the QUIC path and the 1280 default upstream warns about.
- Whether the engine owns a `ChanTUN` or a second device type.
- How registration reports progress when the terminal is not interactive.
- Whether the HTTP/2 fallback is a flag, a profile field, or an automatic fallback.

### Sources and Research

- usque `README.md` and `RESEARCH.md` at revision `6aa03fc97d12848dce34eedbd187fb1077b5d1ea`, for modes, config fields, registration, hooks, and reconnect behaviour.
- usque `api/tunnel.go` and `api/masque.go` for `MaintainTunnelConfig`, `MaintainTunnel`, `ConnectTunnel`, and `PrepareTlsConfig`.
- `docs/architecture.md` for the route policy, DNS policy, address model, packet path, and reconciliation contract this work extends.
- `docs/profile-management.md` and `docs/wireguard.md` for the closest existing non-Tailscale profile kind.
