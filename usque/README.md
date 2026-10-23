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

Behavioral differences carried by tailmix are listed in
[Behavioral differences](#behavioral-differences). Until the fork is adapted
into a library, this tree is a verbatim copy of the upstream revision.
