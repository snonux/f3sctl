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
  the two nodes, so a poll routinely lands on the node that did *not* run your
  job — and that node holds a **different** job, quite possibly an old failed
  one. Treat any other id as "no news", not as your result. A client that skips
  this will report healthy shutdowns as failures; it has happened.
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

All of these are optional and may be absent (an operation that has only just
started, or an older server). Render what is there; do not require any of it.
- Use a total timeout of at least **20 minutes** before declaring the job lost.
  The worst case is three hosts × 240 s of guest shutdown, plus the Gogios
  un-mute wait, plus slack. The server independently gives up on a job after 30
  minutes and marks it failed, so a client that waits will eventually be told.

`fans-on` and `fans-off` are **synchronous**: they return `200` with the plug's
state read back from the device. No job, no polling.

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
               "title": "Hosts are still running: the rack fans keep them cool, so switching the plug off now risks overheating. Confirm to proceed." }] }
```

A correct client shows a confirmation with that sentence as its text, and sends
`force=true` only if the user agrees. When the rack is cold the same action
arrives with no `fields` at all, and the client shows a plain button — with no
code that knows the word "force".

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
- **Job state is local to the node that ran the job.** A `POST` may land on
  pi0 and the follow-up `GET /job` on pi1, which has no record of it.

So a client polling a job SHOULD tolerate the job resource reporting
`state: "none"` or a different node's older job. Do not treat that as failure.
The reliable completion signal is the **host state** reaching what you asked
for: after `power-off`, the f-hosts stop answering; after `power-on`, they
start. Use the job for the reason when something goes wrong, and the host state
for whether it worked.

---

## 10. A complete cycle

Discovery, with the rack up:

```http
GET /cgi-bin/f3sctl/ HTTP/1.1
X-API-Key: ...
```
```json
{ "class": ["f3sctl"],
  "properties": { "apiVersion": 1, "version": "v0.1.0", "node": "pi0" },
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

**Stable — a client may rely on these:**

- `rel` names: `self`, `status`, `fans`, `job`, `describedby`, `up`
- action `name`s: `power-on`, `power-off`, `f3-on`, `f3-off`, `fans-on`,
  `fans-off`
- `properties` keys on hosts (`name`, `ip`, `ping`, `ssh`, `ms`), fans (`on`,
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
