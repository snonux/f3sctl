# f3sctl

Control tool for the [f3s homelab](https://f3s.buetow.org): powers the FreeBSD
bhyve hosts `f0`–`f3` on and off, reports their state, and switches the rack
fans — from a shell, or over an API a watch app can drive.

It replaces the `wol-f3s` bash script.

```sh
f3sctl power status          # probe f0-f3 and the k3s nodes
f3sctl power on              # fans on, wake f0/f1/f2, un-mute Gogios
f3sctl power off             # export zusb, mute Gogios, stop guests, power off
                             # fans off too, but only once f0/f1/f2 are silent
                             # — f3 is racked separately and isn't cooled by
                             # this plug, so it plays no part in the guard
f3sctl power f1 on|off       # any single host: f0, f1, f2 or f3
f3sctl fans status|on|off    # the rack-fan Shelly plug on its own
```

**Shutdowns always go through the API**, from anywhere including a Pi. Only
pi0/pi1 can actually perform one — the restricted SSH key is pinned to them —
so routing through the API is what makes the same command work on a laptop.
Waking, status and the fan plug stay local: a magic packet is an unprivileged
broadcast any LAN host may send, which also leaves a way to wake the rack when
the API itself is unreachable. `--local` overrides the routing and `--remote`
forces the API for everything.

Add `--verbose` to trace every API call:

```
$ f3sctl -v power status
f3sctl: API base https://f3s.buetow.org/cgi-bin/f3sctl/
  → GET https://f3s.buetow.org/cgi-bin/f3sctl/
  ← 200 from pi0.lan.buetow.org (510ms)
  → GET https://f3s.buetow.org/cgi-bin/f3sctl/status
  ← 200 from pi1.lan.buetow.org (425ms)
```

That trace answers a question nothing else can: **which node served each
request**. relayd load-balances pi0 and pi1, so consecutive calls land on
different machines — which is exactly why a job started on one may be invisible
from the other. Every response carries an `X-F3sctl-Node` header, errors
included.

`f3sctl` never powers a Raspberry Pi. `pi0`/`pi1` are where it runs, and
powering them off would remove the only way to power anything back on.

## One binary, three modes

| Mode | Selected by | Runs on |
|---|---|---|
| CLI | default | earth, pi0, pi1 |
| CGI | `GATEWAY_INTERFACE` is set | pi0, pi1 (under bozohttpd) |
| agent | `f3sctl agent` | f0–f3 |

f3sctl is deliberately **not** installed on the OpenBSD gateways. They only
ever needed to set and clear the Gogios mute marker, and putting a whole
homelab power tool on the two internet-facing hosts to touch one file is more
surface than that deserves. They run the standalone `f3s-gogios-mute` script
(in the `conf` repo) behind the same forced-command arrangement, speaking the
same bare verbs.

Keeping them in one binary is deliberate: a subsystem added to the route
registry gains a CLI verb and an HTTP route at once, and the API can never
disagree with the CLI about what "power off" means — the API literally runs
`f3sctl power off` as a detached child.

## What `power off` actually does

Ordered so that everything which can refuse does so *before* anything
irreversible happens:

1. **Unmount local NFS** — abort if anything is stuck. The exports come from
   the CARP VIP on f0/f1; powering those off under a live mount hangs the
   client and loses writes in flight.
2. **Export `zusb` where it is imported** — the removable backup pool gets its
   snapshot, clean export and disk spin-down instead of having USB power cut
   from under it. A no-op on the hosts that do not have it.
3. **Mute Gogios** — so a deliberate shutdown does not page.
4. **Stop the CARP failover daemons** — `devd` and `cron` on `f0` and `f1`,
   so nothing reacts to a host receiving the storage VIP while it is shutting
   down, and f0's minutely auto-failback stops promoting. (Stopping the
   daemons, rather than `carp auto-failback disable`, because that command's
   block file lives on the NFS dataset and would outlive the reboot.) A host
   that takes the VIP runs `carpcontrol.sh`,
   which starts NFS on a machine that is seconds from powering off; that is
   what wedged f1 on 2026-08-08 — powered on, off the network, and unwakeable.
   The NFS daemons themselves are deliberately left running: the guests
   elsewhere are still writing to that export while they stop.
5. **Stop guests, then power off** — every host except the storage master
   **in parallel**, then the master on its own. Stopping those daemons is what makes
   the batch safe; the master still goes last for an unrelated reason, namely
   that the other hosts' guests mount their PVs from the VIP over NFS and are
   still using them while they stop. If those daemons could *not* be stopped, the run
   falls back to the old one-at-a-time order. Guests get two SIGTERMs and
   240 s, then SIGKILL. That bound must stay below the hosts'
   `rcshutdown_timeout` (300 s): if `rc.shutdown` overruns its watchdog, init
   drops the host to single-user *still powered on with no network*, and
   Wake-on-LAN cannot wake a NIC that never powered down. Recovering from that
   needs physical access.
6. **Confirm they actually went down** — accepting a shutdown is not the same
   as completing one. Any host still answering ICMP after two minutes is
   reported as a failure, because it is powered on, off the network, and
   Wake-on-LAN will not wake it.
7. **Fans off** — only once f0/f1/f2 are genuinely down. The plug does not
   cool f3, so a running f3 does not keep the fans on.

Every run ends with a timing summary — each stage and each host, longest
called out — so "why did that take so long" is a question the log answers.

## The API

Self-describing hypermedia (Siren) served by bozohttpd on pi0/pi1, reachable at
`https://f3s.buetow.org/cgi-bin/f3sctl/`. A client hard-codes the base URL and
an API key; everything else it discovers.

Actions are advertised **only when they are currently possible** — no
`power-on` when everything is already up, nothing at all while a job is
running, and a confirmation field on `fans-off` only while a host is still
drawing power. A client renders what it is handed and never encodes a rule of
its own.

**How it fits together: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)** — the
three modes, the packages, a shutdown end to end, and the two-node story.

**Writing a client: [`docs/CLIENT.md`](docs/CLIENT.md)**, with a dependency-free
reference implementation in [`docs/client-reference.js`](docs/client-reference.js).

**Managing API keys: [`docs/API-KEYS.md`](docs/API-KEYS.md)** — generating,
adding, listing, removing and rotating the keys the server accepts.

Authentication is an `X-API-Key` header, compared in constant time. It is never
accepted in the query string — bozohttpd logs request URIs to syslog and relayd
logs connections, so a key in a URL would be written to two logs on three
machines.

The key file (`api_key_file`) may list **several accepted keys, one per line**.
Issuing a new client its own key is then a matter of appending a line on each
API node (pi0 *and* pi1 — the key file must match on both, since relayd
load-balances them); revoking one is deleting its line. Blank lines and
`#`-comment lines are ignored, so each key can be labelled with the client it
belongs to. The file is re-read on every request, so neither change needs a
restart. A single-line file behaves exactly as before.

## Security model

The API host reaches the f-hosts over SSH with a key that can do exactly one
thing. On every target:

- a dedicated unprivileged **`f3sctl` user**, whose `authorized_keys` lives in
  a root-owned path outside its home so the account cannot re-authorise itself;
- `from="…"` pinning the key to pi0/pi1, on both their LAN and WireGuard
  addresses;
- `ForceCommand /usr/local/bin/f3sctl agent` in `sshd_config`, which overrides
  whatever the key file says — the restriction is root-owned daemon config, not
  a key option;
- an allowlist of five single-word verbs, none of which takes an argument, so
  there is nothing a key holder can vary;
- `doas` rules keyed to exact argv for the four verbs that need root.

On pi0/pi1 the CGI needs **no** privilege escalation at all: Wake-on-LAN is an
unprivileged UDP broadcast, ICMP comes from the setuid `/sbin/ping`, and the
SSH calls use an explicit identity. Nothing runs as root.

## Building

Uses [mage](https://magefile.org).

```sh
mage build      # ./f3sctl for this platform
mage test       # unit tests
mage install    # -> ~/bin/f3sctl
mage cross      # netbsd/arm64 and freebsd/amd64 into ./dist
mage publish    # package and upload both to pkgrepo.f3s.buetow.org
```

`mage publish` drives `~/git/conf/packages/Makefile`, which is shared with
gogios and dtail and owns the repo layout and build hosts. Note that
`pkgrepo.f3s.buetow.org` is served *from the k3s cluster*, so installing or
updating f3sctl needs the cluster up — you cannot update the tool while the
thing it powers on is off.

## Configuration

Compiled-in defaults, overlaid by `/usr/local/etc/f3sctl.json` if present, so
the tool works with no configuration at all. Path settings are lists and the
first readable entry wins, which lets one shipped config serve both the
`_httpd` CGI and a human running the CLI on the same Pi.

```json
{
  "ssh_identity":         ["/var/db/f3sctl/id_ed25519", "~/.ssh/id_ed25519"],
  "shelly_password_file": ["/var/db/f3sctl/shelly_plug", "~/.shelly_plug"],
  "api_key_file":         "/var/db/f3sctl/apikey",
  "state_dir":            "/var/db/f3sctl",
  "peer_nodes":           ["192.168.1.125", "192.168.1.126"]
}
```

Host inventory (IPs, MACs, the broadcast address, the Shelly plug) lives in
`internal/inventory` and can be overridden by the same file.

`peer_nodes` are the API's own other CGI nodes (pi0 and pi1) that
`internal/coordination.PeerSet` asks "are you mid-job?" before starting one,
so that relayd load-balancing two clicks apart can't start two conflicting
jobs. Readdressing a Pi, adding a third API node, or moving the API elsewhere
is therefore a config edit here, not a rebuild.

The URL path used for that check (`peer_job_path`) is left out of the example
above on purpose: by default it is derived from this node's own SCRIPT_NAME
plus its own `/job` route, on the assumption that both peers are mounted the
same way -- so remounting the CGI script (changing SCRIPT_NAME) keeps every
href *and* the peer check in sync automatically. Set `peer_job_path`
explicitly only if the two peers are genuinely mounted differently.
