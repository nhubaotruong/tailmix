# Cloudflare WARP profiles

tailmix can run a Cloudflare WARP tunnel as a profile of kind `warp`. The engine
is a userspace MASQUE Connect-IP client. It is a profile beside the Tailscale
and raw WireGuard kinds: it has its own profile ID, state directory,
credentials, runtime, and packet device, and one daemon reconciles it with the
others.

WARP profiles are available on Linux in TUN mode. They have no Tailscale
coordination, MagicDNS, or peer list. The tunnel carries the profile's traffic
to Cloudflare; it does not join a tailnet.

## Register a profile

Register with a consumer account:

```sh
sudo tailmix warp register home
```

The command asks for Cloudflare Terms of Service consent before the daemon
performs the enrollment call. A refusal writes no profile and contacts
Cloudflare zero times.

Enroll a ZeroTrust device with a team token from a file:

```sh
sudo tailmix warp register corp --jwt-file ./team.jwt
```

The token is read from the file so it never appears in the argument list.
Registration, the profile write, and the first start happen in one request.

## Import a configuration

An existing `config.json` written by the usque CLI can become a profile without
contacting Cloudflare:

```sh
sudo tailmix warp register home --config ./config.json
```

The file must carry the MASQUE endpoint fields and both tunnel addresses. The
imported file also carries the private key; delete it after the profile starts.
Stop any other WARP client before the profile starts so the two do not compete
for the tunnel.

## Options

`--dns <address>` sets the resolver the profile contributes while it is the
selected exit node. The default is `1.1.1.1`.

`--transport quic|http2` selects the tunnel transport. The default is QUIC. The
HTTP/2 path needs a configuration that provides an HTTP/2 endpoint.

## Inspect a profile

```sh
tailmix profiles show home
tailmix profiles show home --json
tailmix status
```

The profile view shows the kind, the runtime and backend state, whether the
profile holds the default route, the tunnel addresses Cloudflare assigned, and
the time since the last successful connection. A WARP profile has no peers, so
its peer list is empty. The credentials themselves are never printed; only
whether usable credentials exist.

## Routing

A WARP profile advertises `0.0.0.0/0` and `::/0`. Selecting it as the exit node
gives it the default route for both families. An explicit `routes bind` for a
more specific prefix still wins over that default. A profile that is not the
selected exit node carries only the prefixes an explicit binding sends to it.

A selected WARP profile that is not connected keeps the default route as a drop.
The host's physical default does not take over, so traffic cannot leave outside
the tunnel unnoticed. Only one profile holds the default route: selecting a
WARP exit node clears a tailnet exit-node selection, and the reverse.

## DNS

While a WARP profile is the selected exit node it contributes one resolver as
the root `.` DNS route, and lookups on that route dial through the profile. A
MagicDNS suffix from a tailnet still wins over the root route by longest
suffix. Unselecting the profile removes its route.

A selected profile that is not connected keeps the route and answers SERVFAIL,
so a lookup never falls through to a resolver outside the tunnel.

## Credentials and repair

The profile stores its private key, registration ID, access token, license, the
endpoint public key, the endpoint addresses, and the tunnel addresses inside its
own state directory. The private key and access token files are readable only
by their owner.

A profile without usable credentials reports the backend state
`NeedsReRegistration` and opens no tunnel. Register the same name again to
replace the stored credentials. The replacement is atomic, so an interrupted
re-registration leaves the previous credentials in place.

## Lifecycle

Enable, disable, rename, alias, and remove a WARP profile with the ordinary
`profiles` commands. A plain removal keeps the credentials and the state
directory, and a purge deletes them. Neither revokes the Cloudflare
registration.

Creating a WARP profile on another platform fails with
`warp_unsupported_platform`. A state file that names a WARP profile still loads
there, that profile reports the platform error, and the other profiles keep
working.
