# Managing f3sctl API keys

The f3sctl API authenticates every request with an `X-API-Key` header. The
accepted keys live in a single file on each API node (pi0 and pi1), one key per
line. This document is the operator runbook for that file: generating a key,
adding it, listing what's there, removing one, and rotating everything.

It is the counterpart to [`CLIENT.md`](CLIENT.md), which is normative for
*clients* (a client embeds one key and sends it). This is normative for the
*operator* who decides which keys those are.

---

## 1. Where the keys live

| Thing | Value |
|---|---|
| Config key | `api_key_file` in `/usr/local/etc/f3sctl.json` |
| Default path | `/var/db/f3sctl/apikey` |
| Format | one accepted key per line |
| Permissions | `0600`, owned by the user the CGI runs as |
| Nodes | **both** pi0 (`192.168.1.125`) and pi1 (`192.168.1.126`) |

The file is plain text. Each non-blank, non-comment line is one accepted key,
with surrounding whitespace trimmed. A line beginning with `#` is a comment and
is ignored — use it to label which client a key belongs to:

```
# pebble watchface (earth, 2026-08)
9f3a7c1e...
# laptop CLI (earth, 2026-08)
c71e0bd4...
```

A single-line file is a list of length one, so a deployed `apikey` file with
one key in it keeps working unchanged after the multi-key support landed.

### Why both nodes, and why they must match

relayd load-balances pi0 and pi1, so consecutive requests land on different
nodes. Each node reads **its own** key file; there is no shared state. If the
files disagree, a key accepted on pi0 is rejected on pi1 — which looks to a
client like "sometimes my key works, sometimes it doesn't", because it depends
on which node relayd chose. Keep the two files byte-for-byte identical.

### No restart, ever

The server re-reads the key file on **every request** (this is pinned by
`TestAuthenticatorRereadsTheKeyFile` and `TestAuthenticatorRevocationTakesEffectWithoutRestart`
in `internal/httpapi/auth_test.go`). Adding, removing, or rotating a key takes
effect on the very next request. Do not restart bozohttpd.

---

## 2. Generate a key

A key is an opaque random string. Generate one with either of:

```sh
openssl rand -hex 32        # 64 hex chars, no shell-special characters
head -c 32 /dev/urandom | base64 | tr -d '\n'   # 44 base64 chars
```

`openssl rand -hex 32` is preferred: hex contains no characters that shell,
JSON, or HTTP header parsing will mangle, so the key never needs quoting or
escaping anywhere it is stored or sent.

The key is **never** accepted in the query string — bozohttpd logs request
URIs to syslog and relayd logs connections, so a key in a URL would be written
to two logs on three machines. It is only ever read from the `X-API-Key`
header. Keep that in mind when choosing where to store it on the client side
(see `CLIENT.md` §1).

---

## 3. Add a key (issue a new client)

On **both** pi0 and pi1, append the new key with a `#`-comment label:

```sh
NEWKEY=$(openssl rand -hex 32)
doas sh -c 'printf "# %s (%s)\n%s\n" "$LABEL" "$(date +%Y-%m-%d)" "$NEWKEY" >> /var/db/f3sctl/apikey'
```

For example:

```sh
LABEL="pebble watchface"
NEWKEY=$(openssl rand -hex 32)
doas sh -c 'printf "# %s (%s)\n%s\n" "'"$LABEL"'" "$(date +%Y-%m-%d)" "'"$NEWKEY"'" >> /var/db/f3sctl/apikey'
```

Then hand `$NEWKEY` to the client (over a channel you trust — it is a credential,
not something to paste in chat or commit to a repo).

Verify both nodes accept it:

```sh
# from anywhere that can reach the API
curl -sS -H "X-API-Key: $NEWKEY" https://f3s.buetow.org/cgi-bin/f3sctl/ -o /dev/null -w '%{http_code}\n'
# 200 means accepted; 401 means the key was not added (or not on the node that answered)
```

Run the curl a few times — relayd will round-ro-bin between pi0 and pi1, so
repeated calls exercise both nodes. A 401 on any call means the file on that
node does not yet contain the key.

### The key file on a client

On the client, the key goes where the client's platform keeps secrets; for the
`f3sctl` CLI itself, that's the file `api_key_file` points at (or the
`F3SCTL_KEY` environment variable). A client file holds **only the client's own
key** — the first non-blank, non-comment line is what the client sends. Do
**not** copy the server's multi-key file onto a client: every client would then
send the first line (the same key), which defeats the point of per-client keys.

---

## 4. List the keys

The file is plain text, so listing it is just reading it:

```sh
doas cat /var/db/f3sctl/apikey
```

To see just the keys (no comments, no blanks):

```sh
doas grep -vE '^\s*(#|$)' /var/db/f3sctl/apikey
```

Count them:

```sh
doas grep -cvE '^\s*(#|$)' /var/db/f3sctl/apikey
```

If pi0 and pi1 should ever be suspected out of sync, diff them:

```sh
ssh pi0 'doas cat /var/db/f3sctl/apikey' | sort > /tmp/k0
ssh pi1 'doas cat /var/db/f3sctl/apikey' | sort > /tmp/k1
diff /tmp/k0 /tmp/k1
```

---

## 5. Remove a key (revoke a client)

Delete the key's line on **both** pi0 and pi1. The safe way is to rewrite the
file without that line, so you never leave a half-deleted line:

```sh
OLDKEY="c71e0bd4..."
doas sed -i '' "/^${OLDKEY}$/d" /var/db/f3sctl/apikey     # NetBSD sed
# or, on a GNU-sed host:
doas sed -i "/^${OLDKEY}$/d" /var/db/f3sctl/apikey
```

Deleting by matching the whole line (`^…$`) is deliberate: it removes the key
but leaves the `#` comment that labelled it, so the file retains a record of
which client was revoked and when. If you would rather remove the label too,
delete the comment line in the same edit.

Removing a key takes effect on the next request, with no restart. Confirm the
revoked key is now refused and an unrelated key still works:

```sh
curl -sS -H "X-API-Key: $OLDKEY" https://f3s.buetow.org/cgi-bin/f3sctl/ -o /dev/null -w '%{http_code}\n'
# 401

curl -sS -H "X-API-Key: $GOODKEY" https://f3s.buetow.org/cgi-bin/f3sctl/ -o /dev/null -w '%{http_code}\n'
# 200
```

A revoked client sees `401` with `{"class":["error"],"properties":{"status":401,"message":"..."}}`.
Missing and wrong keys are deliberately indistinguishable (see `CLIENT.md` §7),
so a revoked client cannot tell "my key was removed" from "my key is wrong" —
both look the same, by design.

---

## 6. Rotate all keys (replace the set)

Rotation replaces every key at once, which invalidates every existing client.
Do it when the set may be compromised, or as a periodic hygiene step.

```sh
# On both pi0 and pi1, replace the file with a fresh set. Label each one.
doas sh -c 'umask 077 && {
  printf "# pebble watchface (%s)\n" "$(date +%Y-%m-%d)"; openssl rand -hex 32
  printf "# laptop CLI (%s)\n"      "$(date +%Y-%m-%d)"; openssl rand -hex 32
} > /var/db/f3sctl/apikey'
```

`umask 077` keeps the file `0600` while it is being written. The change takes
effect on the next request; no restart.

Then re-issue each client its new key, copied out of the file with
`doas cat /var/db/f3sctl/apikey`. Every client that still holds an old key
will start getting `401` until it is updated.

### Never end up with zero keys

If the file exists but contains only blank lines and comments, the server
refuses **every** request (pinned by `TestAuthenticatorEmptyFileRejectsAll`).
It never falls back to "no key required". So an empty edit in the middle of a
rotation leaves the API locked down rather than open — which is the safe
direction to fail. To avoid that window, write the new file in one step (as
above) rather than clearing it and appending lines one at a time.

---

## 7. How it is implemented

The comparison is the one piece of this package that is a security control, so
it lives in its own file, `internal/httpapi/auth.go`, tested in
`auth_test.go`:

- `Authenticator.Check` reads the file, calls `parseKeys` to split it into
  accepted keys (skipping blank and `#`-comment lines, trimming whitespace),
  and compares the request key against each with `subtle.ConstantTimeCompare`.
  A match returns `nil`; falling through every key returns `bad API key`.
- Each comparison is constant-time, so the loop cannot be turned into an
  oracle for guessing a key one byte at a time. Returning on the first match
  only tells a valid client its own key was accepted, which it already knows;
  an invalid client falls through every comparison.
- The file is read on every `Check` — never cached — which is why adding,
  revoking, and rotating take effect without a restart.

The client side, `config.ResolveAPIKey` in `internal/config/config.go`,
returns the **first** non-blank, non-comment line of the same file: a client
presents one key (its own), regardless of how many the server accepts.