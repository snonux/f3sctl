#!/usr/bin/env node
//
// Reference f3sctl API client.
//
// It knows two things: the base URL and the API key. Every path it fetches
// came out of a response -- grep this file for a literal path such as
// "/status" and you will not find one. That is the property that makes a
// client survive server changes, and it is what docs/CLIENT.md asks for.
//
// Dependency-free and small enough to adapt for PebbleKit JS, where fetch()
// becomes XMLHttpRequest and the CLI wrapper at the bottom is replaced by
// AppMessage handlers.
//
//   F3SCTL_URL=https://f3s.buetow.org/cgi-bin/f3sctl/ F3SCTL_KEY=... \
//     node docs/client-reference.js status|on|off|fans-on|fans-off
//
// It also serves as the executable check that the shipped API matches the
// documented one: `selftest` asserts the discovery rules hold.

'use strict';

const BASE = process.env.F3SCTL_URL;
const KEY = process.env.F3SCTL_KEY;

// The one apiVersion this client understands. Per CLIENT.md section 2, a
// client must stop rather than guess when the server speaks a newer one.
const SUPPORTED_API_VERSION = 1;

// resolve turns an href from a response into an absolute URL.
//
// The server returns root-relative hrefs ("/cgi-bin/f3sctl/status") because it
// sits behind relayd and cannot know the public scheme and hostname the client
// used. Resolving them against the base is the client's job, per RFC 3986 and
// section 3 of CLIENT.md -- and note this is still not "building a path": the
// path came from the server, only the origin is ours.
function resolve(href) {
  return new URL(href, BASE).toString();
}

async function request(href, method = 'GET', fields = null) {
  const url = resolve(href);
  const opts = { method, headers: { 'X-API-Key': KEY } };
  if (fields) {
    opts.headers['Content-Type'] = 'application/x-www-form-urlencoded';
    opts.body = new URLSearchParams(fields).toString();
  }

  const res = await fetch(url, opts);
  const body = await res.json().catch(() => null);

  if (res.status === 401) throw new Error('API key rejected');
  // A 409 means we acted on stale state. Re-fetching is the whole remedy;
  // retrying the same request is not (CLIENT.md section 8).
  if (res.status === 409) throw new Error(`stale state: ${body?.properties?.message ?? res.status}`);
  if (!res.ok && res.status !== 202) {
    throw new Error(body?.properties?.message ?? `HTTP ${res.status}`);
  }
  return body;
}

// root fetches the entry point and refuses to continue on an apiVersion this
// client was not written against.
async function root() {
  const entity = await request(BASE);
  const version = entity.properties?.apiVersion;
  if (version !== SUPPORTED_API_VERSION) {
    throw new Error(
      `server speaks apiVersion ${version}, this client understands ${SUPPORTED_API_VERSION}`);
  }
  return entity;
}

// follow resolves a link by its rel. Never by path.
function follow(entity, rel) {
  const link = (entity.links ?? []).find((l) => l.rel.includes(rel));
  if (!link) throw new Error(`no link with rel "${rel}"`);
  return link.href;
}

// action finds an offered action by name. Absent means "not available now" --
// not an error to work around (CLIENT.md section 3).
function action(entity, name) {
  return (entity.actions ?? []).find((a) => a.name === name) ?? null;
}

// perform invokes an action, filling its fields generically. Nothing here
// knows what any particular field means; `confirm` decides whether a required
// checkbox is ticked, and the field's own title is what a UI would show.
async function perform(act, confirm = false) {
  const fields = {};
  for (const f of act.fields ?? []) {
    fields[f.name] = f.type === 'checkbox' ? String(confirm) : String(f.value ?? '');
    if (f.required && f.type === 'checkbox' && !confirm) {
      throw new Error(`"${act.name}" needs confirmation: ${f.title}`);
    }
  }
  return request(act.href, act.method, Object.keys(fields).length ? fields : null);
}

function entities(entity, cls) {
  return (entity.entities ?? []).filter((e) => (e.class ?? []).includes(cls));
}

// describe maps the two probe signals onto what a user should be told. The
// third case is deliberately hedged: an off host and one hung in single-user
// are indistinguishable from here (CLIENT.md section 4).
function describe(p) {
  if (p.ping && p.ssh) return 'up';
  if (p.ping) return 'starting…';
  return 'off';
}

async function showStatus() {
  const status = await request(follow(await root(), 'status'));

  for (const h of entities(status, 'host')) {
    const p = h.properties;
    console.log(`${p.name.padEnd(4)} ${describe(p).padEnd(10)} ${p.ping ? p.ms.toFixed(1) + 'ms' : ''}`);
  }

  for (const f of entities(status, 'fans')) {
    // An unreachable plug is "unknown", never "off".
    console.log(`fans ${f.properties.error ? 'unknown' : (f.properties.on ? 'on' : 'off')}`);
  }
  return status;
}

// waitForJob polls until the job stops running.
//
// It tolerates the job resource knowing nothing: relayd load-balances pi0 and
// pi1, so the poll may land on the node that did not run the job (CLIENT.md
// section 9). Host state is the reliable completion signal; the job is how you
// learn *why* something failed.
async function waitForJob(entry, timeoutMs = 20 * 60 * 1000) {
  const href = follow(entry, 'job');
  const deadline = Date.now() + timeoutMs;

  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 10000));
    const job = await request(href);
    const s = job.properties?.state;
    if (s === 'none') continue;
    if (s !== 'running') {
      console.log(`job ${s}${job.properties.error ? ': ' + job.properties.error : ''}`);
      return job;
    }
    console.log('…still running');
  }
  throw new Error('gave up waiting for the job');
}

// run performs a named action.
//
// holderRel names the resource that advertises it. Power and fan actions are on
// the root; the monitoring pair is on the resource the "monitoring" link points
// at, because reading that state costs the server an SSH round trip per gateway
// and so is not folded into every response. Either way the action object is the
// server's -- nothing here builds a path.
async function run(name, confirm, holderRel) {
  const entry = await root();
  const holder = holderRel ? await request(follow(entry, holderRel)) : entry;

  const act = action(holder, name);
  if (!act) {
    // Not an error to route around: the server is saying this cannot be done
    // right now, and the reason is visible in the status.
    console.log(`"${name}" is not available right now.`);
    return showStatus();
  }

  const result = await perform(act, confirm);
  console.log(`${name}: ${result.properties?.state ?? 'ok'}`);

  if (result.properties?.state === 'running') return waitForJob(entry);
  return result;
}

// selftest asserts the contract this client depends on, so a server change
// that would break real clients fails here first.
async function selftest() {
  const entry = await root();
  const status = await request(follow(entry, 'status'));
  const check = (ok, msg) => console.log(`${ok ? 'ok  ' : 'FAIL'} ${msg}`);

  check(typeof entry.properties.apiVersion === 'number', 'root carries apiVersion');
  check(['status', 'fans', 'job', 'monitoring', 'describedby'].every((r) => {
    try { follow(entry, r); return true; } catch { return false; }
  }), 'root links to every documented resource');

  const hosts = entities(status, 'host').map((h) => h.properties);
  check(hosts.length > 0, 'status embeds host entities');
  check(hosts.every((h) => 'ping' in h && 'ssh' in h), 'hosts report ping and ssh separately');

  // The heart of the design: offered actions must match observed state.
  const allUp = hosts.filter((h) => h.name !== 'f3' && h.name.startsWith('f')).every((h) => h.ping);
  check(!!action(entry, 'power-on') === !allUp, 'power-on is offered exactly when something is down');
  check(!!action(entry, 'power-off') === hosts.some((h) => h.name.startsWith('f') && h.name !== 'f3' && h.ping),
    'power-off is offered exactly when something is up');

  const fans = entities(status, 'fans')[0]?.properties ?? {};
  check(!!action(entry, 'fans-off') === (fans.on === true && !fans.error), 'fans-off is offered only when the plug is on');

  const off = action(entry, 'fans-off');
  const anyUp = hosts.some((h) => h.name.startsWith('f') && h.ping);
  if (off) {
    check((off.fields?.length > 0) === anyUp, 'fans-off carries a confirmation field only while a host is up');
  }

  // The mute is reachable on its own, independent of any power action -- the
  // property that stops a stranded mute from being unclearable once the fleet
  // is up and power-on has been withheld.
  const mon = await request(follow(entry, 'monitoring'));
  const muted = mon.properties.muted;
  check(typeof muted === 'boolean', 'monitoring reports a mute state');
  check(!!action(mon, 'monitoring-unmute') === muted, 'unmute is offered exactly when muted');
  check(!!action(mon, 'monitoring-mute') === !muted, 'mute is offered exactly when alerting');
}

const commands = {
  status: showStatus,
  on: () => run('power-on'),
  off: () => run('power-off'),
  'f3-on': () => run('f3-on'),
  'f3-off': () => run('f3-off'),
  'monitoring-mute': () => run('monitoring-mute', undefined, 'monitoring'),
  'monitoring-unmute': () => run('monitoring-unmute', undefined, 'monitoring'),
  'fans-on': () => run('fans-on'),
  'fans-off': () => run('fans-off', true),
  selftest,
};

async function main() {
  if (!BASE || !KEY) {
    console.error('set F3SCTL_URL and F3SCTL_KEY');
    process.exit(2);
  }
  const cmd = commands[process.argv[2] ?? 'status'];
  if (!cmd) {
    console.error(`usage: client-reference.js [${Object.keys(commands).join('|')}]`);
    process.exit(2);
  }
  await cmd();
}

main().catch((err) => {
  console.error(String(err.message ?? err));
  process.exit(1);
});
