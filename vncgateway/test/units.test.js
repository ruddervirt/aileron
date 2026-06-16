'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { TokenFactory } = require('../lib/tokens');
const { validName, parseTwoSegments } = require('../lib/path');
const { SessionRegistry } = require('../lib/registry');

test('token encrypt/decrypt round trip', () => {
  const tf = new TokenFactory();
  const value = { connection: { type: 'vnc', settings: { hostname: '127.0.0.1', port: '5901' } } };
  assert.deepStrictEqual(tf.decrypt(tf.encrypt(value)), value);
});

test('validName parity with Go dns1123', () => {
  for (const ok of ['abc123', 'a', 'build-id-1', 'x9', 'abc-def-123']) {
    assert.ok(validName(ok), `${ok} should be valid`);
  }
  for (const bad of ['', '..', 'a/b', 'UPPER', '-leading', 'trailing-', 'has space', 'q?x']) {
    assert.ok(!validName(bad), `${bad} should be invalid`);
  }
});

test('parseTwoSegments', () => {
  assert.deepStrictEqual(parseTwoSegments('/vncgateway/bid/vm', '/vncgateway/'), ['bid', 'vm']);
  assert.deepStrictEqual(parseTwoSegments('/vncgateway/bid/vm/', '/vncgateway/'), ['bid', 'vm']);
  assert.strictEqual(parseTwoSegments('/vncgateway/bid', '/vncgateway/'), null);
  assert.strictEqual(parseTwoSegments('/vncgateway/bid/vm/extra', '/vncgateway/'), null);
  assert.strictEqual(parseTwoSegments('/other/bid/vm', '/vncgateway/'), null);
  assert.strictEqual(parseTwoSegments('/vncgateway/../etc', '/vncgateway/'), null);
});

test('registry: first connects, second joins, close bookkeeping', async () => {
  let tunnelCalls = 0;
  const reg = new SessionRegistry({
    ensureTunnel: async () => {
      tunnelCalls++;
      return 12345;
    },
    connectSettings: { 'color-depth': '24' },
    joinSettings: { 'read-only': 'false' },
  });

  const first = await reg.acquire('ns', 'vmi');
  assert.strictEqual(first.connection.type, 'vnc');
  assert.strictEqual(first.connection.settings.port, '12345');
  assert.strictEqual(first.vncgwPrimary, true);
  assert.strictEqual(tunnelCalls, 1);

  // Second client arrives before the first opened: it must wait, then join.
  const pendingJoin = reg.acquire('ns', 'vmi');
  const ccPrimary = { guacamoleConnectionId: '$abc' };
  reg.onOpen('ns/vmi', ccPrimary);

  const second = await pendingJoin;
  assert.strictEqual(second.connection.join, '$abc');
  assert.strictEqual(tunnelCalls, 1, 'join must not create another tunnel');

  const ccJoin = { guacamoleConnectionId: '$abc' };
  reg.onOpen('ns/vmi', ccJoin);
  assert.strictEqual(reg.size(), 1);
  assert.ok(reg.has('ns', 'vmi'));

  // Primary leaves; joiner keeps the session alive.
  reg.onClose('ns/vmi', ccPrimary, true);
  assert.strictEqual(reg.size(), 1);
  const third = await reg.acquire('ns', 'vmi');
  assert.strictEqual(third.connection.join, '$abc');

  // Last client leaves; session dropped; next acquire reconnects.
  reg.onClose('ns/vmi', ccJoin, false);
  assert.strictEqual(reg.size(), 0);
  const fourth = await reg.acquire('ns', 'vmi');
  assert.strictEqual(fourth.connection.type, 'vnc');
  assert.strictEqual(tunnelCalls, 2);
});

test('registry: guacd error drops the whole session immediately', async () => {
  const reg = new SessionRegistry({
    ensureTunnel: async () => 12345,
    connectSettings: {},
    joinSettings: {},
  });

  await reg.acquire('ns', 'vmi');
  const ccPrimary = { guacamoleConnectionId: '$abc' };
  reg.onOpen('ns/vmi', ccPrimary);
  const ccJoin = { guacamoleConnectionId: '$abc' };
  reg.onOpen('ns/vmi', ccJoin);

  const err = new Error('guacd error 515: Aborted');
  err.guacdError = true;
  reg.onClose('ns/vmi', ccPrimary, true, err);
  assert.strictEqual(reg.size(), 0, 'session must drop on guacd error');

  // Fresh attempt creates a new connection.
  const retry = await reg.acquire('ns', 'vmi');
  assert.strictEqual(retry.connection.type, 'vnc');
});

test('registry: primary failure before open rejects waiters', async () => {
  const reg = new SessionRegistry({
    ensureTunnel: async () => 12345,
    connectSettings: {},
    joinSettings: {},
  });

  await reg.acquire('ns', 'vmi');
  const pendingJoin = reg.acquire('ns', 'vmi');

  reg.onClose('ns/vmi', { guacamoleConnectionId: null }, true, new Error('guacd refused'));

  await assert.rejects(pendingJoin, /guacd refused/);
  assert.strictEqual(reg.size(), 0);
});

test('registry: tunnel failure clears the slot', async () => {
  let fail = true;
  const reg = new SessionRegistry({
    ensureTunnel: async () => {
      if (fail) {
        throw new Error('bridge down');
      }
      return 999;
    },
    connectSettings: {},
    joinSettings: {},
  });

  await assert.rejects(reg.acquire('ns', 'vmi'), /bridge down/);
  assert.strictEqual(reg.size(), 0);

  fail = false;
  const ok = await reg.acquire('ns', 'vmi');
  assert.strictEqual(ok.connection.settings.port, '999');
});
