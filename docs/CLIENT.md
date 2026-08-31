# Writing an f3sctl API client

This document is **normative**. If a client follows it, it will keep working
across f3sctl releases; if it does not, it will break the first time a path
changes. Where this document and the implementation disagree, one of them is a
bug — the registry tests in `internal/httpapi/registry_test.go` exist to keep
that from happening quietly.

The API is hypermedia (Siren). The short version of everything below:

> **Fetch the root. Render what it offers. Never build a URL.**

---

## 1. The two constants

A client embeds exactly two things:

| Constant | Example |
|---|---|
| Base URL | `https://f3s.buetow.org/cgi-bin/f3sctl/` |
| API key | a long random string |

Everything else — which resources exist, which actions are possible, what they
are called, what fields they take — is discovered at runtime.

The key goes in the **`X-API-Key` request header**. It MUST NOT be put in the
query string: bozohttpd logs request URIs to syslog on the Pi and relayd logs
connections on the gateway, so a key in a URL ends up written to two logs on
three machines. The server does not read it from anywhere but the header.

Store the key wherever your platform keeps secrets. On PebbleKit JS that is the
configuration page's `localStorage`; it is not high-grade storage, which is
part of why the key is scoped to one API and is easy to rotate.

There is no login, no session and no token refresh. Every request carries the
key.

---

## 2. The discovery loop

1. `GET {base}` with the API key.
2. Read `properties.apiVersion`.
3. Follow `links` by their `rel` to reach resources.
4. Render one control per entry in `actions`.

Normatively:

- A client **MUST** send `X-API-Key` on every request.
- A client **MUST** read `properties.apiVersion` before using a response, and
  **MUST** stop with a clear message if it is a version the client does not
  understand. Guessing is worse than refusing.
- A client **MUST** locate resources by `rel` and actions by `name`.
- A client **MUST** use the `href` and `method` exactly as given.
- Hrefs are **root-relative** (`/cgi-bin/f3sctl/status`), because the server
  sits behind relayd and cannot know the public scheme and hostname you
  reached it by. A client **MUST** resolve them against the base URL — RFC
  3986, or one line of `new URL(href, base)`. That is not the same as building
  a path: the path came from the server, only the origin is yours.
- A client **SHOULD** re-fetch after performing an action, because the set of
  available actions will usually have changed.
- A client **MAY** cache the root document for the lifetime of a screen, but
  **MUST NOT** cache it across an action.
- A client **MUST NOT** treat an unknown `class`, `rel`, `name` or field
  `type` as an error. Ignore what you do not understand, or render it
  generically; new ones will be added.

---

## 3. The golden rule

**A client MUST NOT construct a path.**

Not `base + "/power/off"`. Not a switch statement mapping action names to
paths. Read `href` from the action you were handed.

The corollary matters just as much:

**If an action is not in the response, it is not available. Do not attempt it,
and do not encode why.**

The server already knows every rule about when something is possible — whether
a job is running, whether the hosts are already up, whether the plug can be
read. It expresses those rules by including or omitting the action. A client
that keeps its own copy of those rules will be wrong the moment one changes,
and will show a button that only ever produces an error.

**A client that special-cases a 409 is written wrong.** A 409 means it acted on
stale state (see §7).

---

## 4. State: what to show the user

Each host entity in `/status` carries two independent booleans:

| `ping` | `ssh` | State | Suggested display |
|---|---|---|---|
| `true` | `true` | up | "up" |
| `true` | `false` | in transition | "starting…" / "stopping…" |
| `false` | `false` | off, **or hung** | "off" |
| `false` | `true` | — | should not occur; show "up" |

There is a third boolean, `pingKnown`, and it overrides the table when it is
`false`: the server could not carry the ICMP probe out at all — no usable
`ping(8)` where it runs, a probe that could not be started — so `ping: false`
there means "not measured", not "silent". Show **"unknown"**, never "off". The
server treats it the same way: an unmeasured host keeps the rack fans on. A
server too old to send the field omits it; absent means "the probe ran".

The second row is genuinely directionless: a host answering ICMP with no sshd
is either coming up or going down, and one observation cannot tell which. If a
job is running, `job.properties.action` says which way — use that to pick the
wording. With no job, "in transition" is the honest label.

The third row is genuinely ambiguous and a client should not pretend otherwise.
A host that failed to shut down cleanly ends up in single-user mode: powered
on, no network, and **not wakeable by Wake-on-LAN**, because WoL only wakes a
powered-off NIC. It looks identical to "off" from here.

So: if a `power-on` job completes and a host is still `!ping` a few minutes
later, tell the user it may need a physical power cycle rather than letting
them press the button again forever. That is the one piece of domain knowledge
worth putting in a client, and it is about presentation, not about the API.

Overall rack state is best derived as:

- **all off** — no f-host has `ping`
- **partially up** — some do
- **up** — every f-host and every cluster host has `ping` and `ssh`

The `fans` entity carries `on`. If it carries `error`, the plug could not be
reached: show "unknown", **not** "off". They are different situations, and
showing the rack as uncooled when it is merely unreachable will send someone
to the garage for nothing.

### Two power groups

`power-on` / `power-off` act on **f0, f1, f2** — the k3s cluster. f3 is
excluded deliberately: it runs a standalone VM and is usually wanted
independently.

`all-on` / `all-off` act on **every f-host, f3 included**. Same sequence
otherwise: same pre-flight, same Gogios mute, same storage-master-last
ordering, same fans-off once every host is silent.

#### The fans may be left running

The fan plug cools the whole rack, so a shutdown only switches it off once
**no f-host answers at all** — f3 included, and including hosts the run never
touched or could not probe. So `power-off`, which leaves f3 up by design,
normally finishes with the fans **still running**; `all-off` normally does not.

That is a success, not a failure: `rc` is `0` and `state` is `done`. The job's
last `step` says which happened — it starts with `rack fans left ON` when the
plug was deliberately not switched, and names the hosts responsible. A client
that wants to show "rack is cold" must read that, not assume `power-off`
implies it. The same phrase appears in a failed job's `error`, where hosts did
not complete their shutdown and the fans were kept on for the same reason.

A client should not assume one implies the other. With f0–f2 up and only f3
down, `power-on` is absent (the cluster is already up) while `all-on` is
offered. Render whichever the server gives you.

### Monitoring mute

Follow the root's `monitoring` link. It reports whether Gogios alerting is
suppressed, as a top-level `muted` plus one entity per gateway:

```json
{ "class": ["monitoring"],
  "properties": {"muted": true, "node": "pi0"},
  "entities": [
    {"class":["gateway"], "properties":{"name":"blowfish",   "muted":true}},
    {"class":["gateway"], "properties":{"name":"fishfinger", "muted":true}}
  ],
  "actions": [{"name":"monitoring-unmute", "method":"POST",
               "href":"/cgi-bin/f3sctl/monitoring/unmute"}] }
```

A gateway entity carrying `error` instead of `muted` is unreachable. As with
the fan plug, that is **not** the same as "alerting is fine" — show it as
unknown, because the failure mode being guarded against here is believing you
are monitored when you are not.

This resource is **not** folded into `/status`, and that is deliberate: reading
it costs the server an SSH round trip to each gateway, so a watchface polling
status every few seconds must not drag it along. Fetch it when the user asks,
or on a slow timer.

Treat `muted: true` while the fleet is up as a **warning worth surfacing**. It
means a shutdown muted alerting and the un-mute never completed — the fleet is
running with nobody watching it. `monitoring-unmute` is offered exactly then.

### Gogios alerting

Follow the root's `gogios` link for the alert report overview: the subject
headline, `lastUpdated`, and a `summary` of the six counts Gogios itself
reports (`critical`/`warning`/`unknown`/`stale`/`suppressed`/`ok`):

```json
{ "class": ["gogios"],
  "properties": {"subject": "GOGIOS Report [C:1 W:0 U:0 S:0 SU:0 OK:42]",
                 "lastUpdated": "2026-08-27T08:58:18+02:00", "node": "pi0",
                 "summary": {"critical":1,"warning":0,"unknown":0,"stale":0,"suppressed":0,"ok":42}},
  "links": [ {"rel":["critical"], "href":"/cgi-bin/f3sctl/gogios/critical"},
             {"rel":["warning"],  "href":"/cgi-bin/f3sctl/gogios/warning"}, "...",
             {"rel":["monitoring"], "href":"/cgi-bin/f3sctl/monitoring"} ],
  "actions": [{"name":"gogios-cache-clear", "method":"POST",
               "href":"/cgi-bin/f3sctl/gogios/cache/clear"}] }
```

Follow one of the six `rel`s to drill down into that category's checks. Note
`critical`/`warning`/`unknown`/`ok` group by a check's own severity, while
`stale`/`suppressed` group by lifecycle instead — a stale check keeps
whatever severity it already had, so it can legitimately appear under both
its severity link and `stale`. Each check entity carries its own `self` link
with `?name=` already filled in; **do not** build that query string yourself
or reuse the bare route — a check's name is mandatory and the API does not
advertise a root-level link for it (see the per-check `self` link instead).

A `gogios` (or drill-down) entity carrying `error` instead of its usual
properties means the report itself is currently unreachable — same
"unknown, not off" rule as `/fans` and `/monitoring`: render it as unknown,
never as "no alerts". `gogios-cache-clear` is always offered; it clears the
server's on-disk cache and returns the fresh overview, for when an operator
knows Gogios itself just changed and does not want to wait out the cache TTL.

Each check entity — embedded in a drill-down collection or fetched standalone
via its own `self` link — always carries `name`, `status`, `output` and
`epoch`. `prevStatus`, `federatedFrom` and `lastCheckedAgeSeconds` appear only
when Gogios reported a non-empty/non-zero value for them; a check with no
prior status, no federation source, or no last-checked age simply omits that
property rather than sending an empty string or a `0`:

```json
{ "class": ["check"], "rel": ["item"],
  "properties": {"name": "Check Ping6 r1.wg0.wan.buetow.org", "status": "CRITICAL",
                 "output": "connect: no route to host", "epoch": 1787834698,
                 "prevStatus": "OK", "federatedFrom": "fishfinger",
                 "lastCheckedAgeSeconds": 187},
  "links": [ {"rel":["self"],
              "href":"/cgi-bin/f3sctl/gogios/check?name=Check+Ping6+r1.wg0.wan.buetow.org"} ] }
```

A check's `name` mirrors the monitored command, so it can contain spaces,
dots and slashes — the reason it travels as the `?name=` query parameter
(form-encoded, so a space becomes `+`) rather than a path segment. As already
noted above, fetch it from `self` rather than encoding it by hand.

The report is served from an on-disk cache for `GogiosCacheTTL` (60 seconds
by default, operator-configurable): a `/gogios*` read may return a report up
to that old rather than re-fetching, so clicking through overview →
drill-down → check detail in one browse session does not cost Gogios a
request per click. `gogios-cache-clear` bypasses this — it deletes the cache
and re-fetches before returning the fresh overview.

That cache TTL is not the only source of lag in `lastUpdated`: the report
itself is only regenerated by Gogios (a separate tool, on its own cron) —
every 5 minutes, 08:00–22:00 in this deployment. Outside that window, or
between runs, `lastUpdated` can be far older than `GogiosCacheTTL` alone
would suggest. Read `lastUpdated` to judge freshness; do not assume a recent
fetch implies a recent report.

---

## 5. Actions that take time

`power-on` and `power-off` return **`202 Accepted`** with a `job` entity. They
have not finished — `power off` stops guests on three hosts in sequence, each
with a 240-second bound.

```
POST {action.href}          -> 202, {"class":["job"], "properties":{"state":"running", ...}}
GET  {job link}             -> poll
```

- Poll the `job` link every **5–15 seconds**. Faster buys nothing: the
  underlying operation moves on a scale of minutes.
- **Check `properties.id` matches the job you started.** relayd load-balances
  the two nodes, and whichever one answers asks its peer for the job before
  replying (see §9) — but that ask is best-effort: if the two nodes cannot
  reach each other at that moment, a poll can still land on a node holding a
  **different**, older job of its own. Treat any other id as "no news", not as
  your result. A client that skips this will report healthy shutdowns as
  failures; it has happened.
- Stop when `properties.state` is no longer `"running"`. It becomes `"done"` or
  `"failed"`.
- Read `properties.rc` (the exit code) and `properties.error` for the reason.

### Watching an operation advance

A running job carries progress, so a client can show what is happening rather
than a spinner:

```json
{ "class": ["job"],
  "properties": {
    "id": "c8fe5f2131c26ed3", "action": "off", "state": "running",
    "step": "shutting down f2",
    "updated": "2026-08-08T19:12:41Z",
    "hosts": {
      "f1": {"phase": "done",       "detail": "powered off"},
      "f2": {"phase": "confirming", "detail": "accepted; waiting for it to go silent"},
      "f0": {"phase": "pending"}
    } } }
```

- `step` is the stage in prose — safe to display verbatim, not safe to parse.
- `hosts[name].phase` is one of `pending`, `working`, `confirming`, `done`,
  `failed`. `confirming` means the host accepted the shutdown and the server is
  waiting for it to actually stop answering — accepting is not completing.
- `detail` explains the phase when there is something worth saying, most
  usefully on `failed`.
- `updated` is when progress last changed. A job whose `updated` has not moved
  in several minutes is worth surfacing, even while `state` is still
  `"running"` — a host stuck in `confirming` is the signature of one that hung
  and will not wake.

**A `failed` host has two quite different meanings, and `detail` is what
separates them.** Show it; do not summarise it as "failed".

- `still answering after 2m0s; likely hung and not wakeable` — the host took
  the shutdown and kept replying. It is heading for "powered on with no
  network", where Wake-on-LAN cannot reach it. This one needs a console or the
  physical button.
- `never answered again, but could not be probed either; power-down
  unconfirmed` — the server could not run its liveness probe. The hosts went
  quiet and very probably powered off exactly as asked; what failed is the
  probe, on the machine serving this API. Nothing about the rack follows from
  it — including the `ping` flags in `/status`, which are the same probe.

The job is `failed` in both cases, deliberately: the server proved nothing, and
saying so is the point.

All of these are optional and may be absent (an operation that has only just
started, or an older server). Render what is there; do not require any of it.
- `staleAfterSeconds` (present whenever a job entity is, i.e. not on the
  `"state":"none"` shape) is **this node's own ceiling**: how long it lets a
  job claim to be running before deciding the process behind it is gone (the
  node rebooted mid-shutdown, say) and marking it `failed` on your behalf,
  with a fabricated `"the process that owned this job is gone (node
  restarted?)"` error. It is derived from the larger of that node's
  configured `UnmuteTimeout` (the wake path's worst case) and its shutdown
  path's own worst case (host count times `VMShutdownTimeout`, plus the
  power-down confirmation wait), plus a fixed buffer -- **not** a flat 30
  minutes -- so it scales automatically whenever either `UnmuteTimeout` or
  `VMShutdownTimeout` is raised. Prefer deriving
  your own poll deadline from it (`staleAfterSeconds * 1000` plus a minute or
  so of slack for your own poll interval) over hardcoding a client-side
  guess: a hardcoded number is a second, independent copy of the same value
  that can only be kept in sync by an operator remembering to update it by
  hand every time `UnmuteTimeout` changes -- which is exactly the bug that
  first shipped this field (see `docs/client-reference.js`'s `waitForJob` for
  a worked example, and this repo's history for both the client-side and
  server-side incidents that motivated it).
- Fall back to a total timeout of at least **25 minutes** whenever a response
  carries no `staleAfterSeconds` to derive one from -- not only against a
  server old enough not to send it at all (pre-`staleAfterSeconds` servers
  still enforce their own, un-advertised ceiling internally -- historically a
  flat 30 minutes), but also, on an otherwise current server, any poll that
  lands on the `"state":"none"` shape, which carries no job entity and so no
  `staleAfterSeconds` either (see the field's note above). That is not a
  functional problem -- the fallback is the more generous of the two numbers
  in every realistic configuration, so it can only make a client wait longer,
  never give up sooner -- but do not assume seeing the fallback trigger means
  the server is old. That 25-minute floor comes from the Gogios
  un-mute wait (`UnmuteTimeout`, 1200 s / 20 min by default) plus the wake
  prelude (fans, magic packets) and the gateway SSH round trips that follow
  it, which need roughly **5 minutes** of slack on top of `UnmuteTimeout`
  since neither is bounded by `UnmuteTimeout` itself. A client that timed out
  at 20 minutes flat used to report "gave up" moments before a job that
  succeeded, because 20 minutes matched `UnmuteTimeout`'s default with no
  slack at all -- the same false-negative class a hardcoded, unsynced client
  timeout can still produce today if `staleAfterSeconds` is ignored: the
  server's own ceiling now moves with `UnmuteTimeout`, so a client that never
  re-derives its guess after an operator raises `UnmuteTimeout` will
  eventually give up while the server-reported job is still healthy.

`fans-on` and `fans-off` are **synchronous**: they return `200` with the plug's
state read back from the device. No job, no polling.

`fans-off` is the one slow request in this API. Before it cuts the cooling it
re-probes the rack, and proving that a silent host really is powered off takes
several pings spaced ten seconds apart — so a request that is about to succeed
can take the better part of a minute. Allow **60 s** for it. (`fans-off` with
`force=true`, and every other route, answers in the usual few seconds.)

---

## 6. Rendering fields

An action may carry `fields`. Render them generically:

| `type` | Render as |
|---|---|
| `checkbox` | a toggle or confirmation |
| anything else | a text input |

Use the field's **`title` as the label**. Do not write your own wording for a
field you think you recognise — the title explains the *current* reason the
field is there, and that reason is not always the same.

Send fields as `application/x-www-form-urlencoded` using the action's `type`.

**Always send a `Content-Length` on POST, even when the action takes no fields.**
bozohttpd rejects a POST with no `Content-Length` header at all with a **400**,
before the CGI ever runs — so the error looks like the API refusing the action
when in fact the API never saw it. Most HTTP libraries send `Content-Length: 0`
automatically (`fetch`, PebbleKit JS, Go's net/http all do); `curl -X POST`
with no `--data` does not, which is the usual way to trip over this.

Worked example. While a host is running, `fans-off` arrives as:

```json
{ "name": "fans-off", "title": "Switch the rack fans off",
  "method": "POST", "href": "/cgi-bin/f3sctl/fans/off",
  "type": "application/x-www-form-urlencoded",
  "fields": [{ "name": "force", "type": "checkbox", "value": false,
               "required": true,
               "title": "Hosts may still be running (f3 still running): the rack fans keep them cool, so switching the plug off now risks overheating. Confirm to proceed." }] }
```

A correct client shows a confirmation with that sentence as its text, and sends
`force=true` only if the user agrees. When the rack is cold the same action
arrives with no `fields` at all, and the client shows a plain button — with no
code that knows the word "force".

The parenthesis is the reason, and it varies. `f3 still running` is the rack
working as intended. `f3 could not be probed, so assumed running` means the
server could not establish anything about that host and is refusing to guess —
in that state the `ping` flags in `/status` are not evidence of an idle rack
either, so do not present it as one. This is why §6 says to render the title as
given instead of writing your own.

**`fans-off` is judged twice, on two different budgets, and they can
disagree.** The `force` field's presence is decided by a cheap single-probe
snapshot taken once for the whole response; the plug switch itself is guarded
by a slower, stricter, multi-probe confirmation run only when the request
actually tries to flip it. When the snapshot reads the rack as cold, the field
is omitted — but the confirming probe can still find a host up, and a
`fans-off` sent **without** `force` then comes back `409` even though nothing
in the response ever offered the field to withhold.

Do not treat this 409 like the others in §7 (re-fetch and re-render, hope the
next snapshot agrees with the probe that just ran). It is not guaranteed to
resolve itself in any particular number of retries, and re-fetching throws
away information your user already gave you: if they explicitly asked for
`fans off` **and** confirmed the force checkbox last time it was offered, send
`force=true` on the retry directly, whether or not this response's `fields`
array happens to list it. The server's own multi-probe confirmation is the
real safety gate, not the advertisement — sending an unsolicited-but-genuine
`force=true` can never make the action less safe, it only avoids a second
round trip for a "yes" the user already gave. Never fabricate `force=true`
when the user has not actually confirmed it; that would skip the confirmation
this whole field exists to get.

---

## 7. Errors

| Status | Meaning | What a client should do |
|---|---|---|
| `401` | Missing or wrong API key | Say the key is wrong. Do not retry; it will not start working. Missing and wrong are deliberately indistinguishable. |
| `404` | No such resource | You built a URL. See §3. |
| `405` | Wrong method | You did not use the action's `method`. |
| `409` | Not available now, or a job is already running | **Re-fetch and re-render.** Never retry blindly. |
| `502` | The plug or a host could not be reached | Show the message; it is about the homelab, not the request. |
| `500` | The API is misconfigured | Show the message; it needs an operator. |
| network failure | pi0/pi1, the gateway, or the uplink is down | **Not** the same as "the cluster is down" — see below. |

Errors use the same envelope as everything else, so one parser suffices:

```json
{ "class": ["error"], "properties": { "status": 409, "message": "..." } }
```

The last row deserves care. If the request itself fails, you have learned
nothing about the f-hosts — only that you could not ask. Show "unreachable",
not "off". Reporting the cluster as down because the phone lost signal is the
most likely wrong thing a client will do.

---

## 8. Races between clients

Two watches, two phones, and a laptop shell can all act at once. The server
serialises power operations with a lock and does **not** queue: the second
caller gets `409`.

The correct response to a `409` is always the same: **re-fetch the resource and
re-render**. The new response will show the running job and will not offer the
power actions, which is exactly the state the user should see.

Fan actions are idempotent in effect — switching an already-on plug on changes
nothing — but they are not offered when they would be no-ops, so a client
following §3 will not send them.

---

## 9. Two nodes serve this API

relayd load-balances the two Raspberry Pis, pi0 and pi1. Consecutive requests
may land on different nodes. Every response says which one answered, in the
**`X-F3sctl-Node` header** — present on every reply including errors, which
have no entity properties to carry it. Entity bodies also carry
`properties.node`.

Logging that header per request is the single most useful thing a client can do
for its own debuggability; `f3sctl --verbose` exists for exactly this.

What this means:

- **Host and fan state are authoritative from either node.** Both probe the
  same network and get the same answer.
- **Job state is asked for from both nodes and merged.** A `POST` may land on
  pi0; the follow-up `GET /job` on pi1 asks pi0 for its job before answering,
  so it reports the same job pi0 would -- a running job always wins over a
  merely finished one, and otherwise whichever job started more recently
  does. This costs one extra, short (3s-bounded) request behind the scenes;
  it is not visible on the wire as anything but `GET /job` taking slightly
  longer when it has to happen.

That merge is best-effort, not a guarantee: **when a node cannot reach its
peer at all** — the peer is down, or the LAN path between the two Pis is cut,
as opposed to relayd merely routing a request to one or the other — `GET
/job` falls back to answering from local state alone, which may be
`state: "none"` or an older job than the one you started. A client polling a
job SHOULD still tolerate that. The reliable completion signal is the **host
state** reaching what you asked for: after `power-off`, the f-hosts stop
answering; after `power-on`, they start. Use the job for the reason when
something goes wrong, and the host state for whether it worked.

---

## 10. A complete cycle

Discovery, with the rack up:

```http
GET /cgi-bin/f3sctl/ HTTP/1.1
X-API-Key: ...
```
```json
{ "class": ["f3sctl"],
  "properties": { "apiVersion": 1, "version": "v0.7.0", "node": "pi0" },
  "links": [ { "rel": ["status"], "href": "/cgi-bin/f3sctl/status" }, ... ],
  "actions": [
    { "name": "power-off", "method": "POST", "href": "/cgi-bin/f3sctl/power/off" },
    { "name": "f3-on",     "method": "POST", "href": "/cgi-bin/f3sctl/power/f3/on" },
    { "name": "fans-off",  "method": "POST", "href": "/cgi-bin/f3sctl/fans/off", "fields": [...] } ] }
```

Note what is **not** there: no `power-on` (everything is already up), no
`fans-on` (already on). The client renders three buttons because it was given
three actions.

Shut down:

```http
POST /cgi-bin/f3sctl/power/off HTTP/1.1
X-API-Key: ...
```
```json
{ "class": ["job"],
  "properties": { "action": "off", "state": "running", "node": "pi0", "rc": null } }
```
→ `202`. Poll `/status`; the f-hosts lose `ssh`, then `ping`. Meanwhile the
root offers **no** power actions at all, because a job is running.

When it finishes, the root offers `power-on` and `fans-on`, and nothing else.

---

## 11. Versioning

`properties.apiVersion` is an integer. It changes only on a breaking change.
The current version is **1** and has been since the API's first release: the
server's reorganisation into per-domain surface packages (power, Gogios)
moved only Go code, no response or action shape, so the version never moved
with it. A brief bump to 2 for that same no-op restructure was reverted —
there was nothing on the wire for old clients to trip over. Third-party REST
clients written against v1 keep working unchanged. A client MUST still treat
an `apiVersion` it does not recognise as a stop signal (§2): the refusal is
there for the day a shape actually changes, so please keep honouring it. If
you pin client and server versions together — the deployment model this
project assumes — you will never see the refusal anyway.

**Stable — a client may rely on these:**

- `rel` names: `self`, `status`, `fans`, `job`, `describedby`, `up`
- action `name`s: `power-on`, `power-off`, `f3-on`, `f3-off`, `fans-on`,
  `fans-off`
- `properties` keys on hosts (`name`, `ip`, `ping`, `pingKnown`, `ssh`, `ms`), fans (`on`,
  `ip`, `error`) and jobs (`action`, `state`, `started`, `finished`, `rc`,
  `node`, `error`)
- job `state` values: `running`, `done`, `failed`
- the `X-API-Key` header and the error envelope

**Not stable — a client must not depend on these:**

- any path or `href` (this is the whole point)
- the order of `links`, `actions`, `entities` or `fields`
- which actions appear (that is state, by design)
- `title` text
- **new** actions, links, entity classes and properties appearing at any time

A machine-readable description of the surface is at the `describedby` link
(`/openapi.json`), generated from the same registry that serves requests. It
describes what exists in general; the Siren responses describe what is possible
now. When they seem to disagree, the Siren response is the one to act on.

---

## Reference client

`client-reference.js` in this directory is a working client in ~100 lines of
dependency-free JavaScript. It knows the base URL and the key and nothing else
— grep it for a literal path and you will not find one. It is small enough to
adapt for PebbleKit JS, and it doubles as the executable check that this
document describes the API that actually shipped:

```sh
F3SCTL_URL=https://f3s.buetow.org/cgi-bin/f3sctl/ F3SCTL_KEY=... node docs/client-reference.js status
```
