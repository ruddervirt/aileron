'use strict';

// SessionRegistry makes multi-client muxing transparent: the first client for
// a VM creates the guacd connection, every later client JOINS it via the
// Guacamole connection-join feature. KubeVirt allows exactly ONE VNC
// connection per VMI (a new connection kicks the existing one), so all
// viewers MUST share a single upstream connection.
//
// Keyed by "namespace/vmi". Single-replica, in-memory by design.
class SessionRegistry {
  constructor({ ensureTunnel, connectSettings, joinSettings, log }) {
    this.ensureTunnel = ensureTunnel;
    this.connectSettings = connectSettings;
    this.joinSettings = joinSettings || { 'read-only': 'false' };
    this.log = log || (() => {});
    this.sessions = new Map();
    // Connections that reached 'open'; close events for connections that
    // never opened must not decrement the client count.
    this.opened = new WeakSet();
  }

  // acquire returns the guacamole-lite token object (to be encrypted) for a
  // client connecting to ns/vmi: a connect token for the first client, a join
  // token once the primary connection is open. Throws if the primary
  // connection fails before opening.
  async acquire(namespace, vmi) {
    const key = `${namespace}/${vmi}`;
    let session = this.sessions.get(key);

    if (!session) {
      session = { key, clients: 0, guacId: null };
      session.openPromise = new Promise((resolve, reject) => {
        session.resolveOpen = resolve;
        session.rejectOpen = reject;
      });
      // Waiters handle rejection themselves; avoid unhandled-rejection noise
      // when there are none.
      session.openPromise.catch(() => {});
      this.sessions.set(key, session);

      let port;
      try {
        port = await this.ensureTunnel(namespace, vmi);
      } catch (err) {
        this.drop(session, err);
        throw err;
      }

      this.log(`session ${key}: new connection via tunnel port ${port}`);
      return {
        connection: {
          type: 'vnc',
          settings: {
            ...this.connectSettings,
            hostname: '127.0.0.1',
            port: String(port),
          },
        },
        vncgwKey: key,
        vncgwPrimary: true,
      };
    }

    const guacId = await session.openPromise;
    this.log(`session ${key}: joining ${guacId}`);
    return {
      connection: {
        join: guacId,
        settings: { ...this.joinSettings },
      },
      vncgwKey: key,
    };
  }

  // onOpen: a client connection reached guacd 'ready'.
  onOpen(key, clientConnection) {
    const session = this.sessions.get(key);
    if (!session) {
      return;
    }
    this.opened.add(clientConnection);
    session.clients++;
    if (!session.guacId) {
      session.guacId = clientConnection.guacamoleConnectionId;
      session.resolveOpen(session.guacId);
      this.log(`session ${key}: open as ${session.guacId}`);
    }
  }

  // onClose: a client connection ended. The session lives while any client
  // remains (the primary leaving with joiners attached keeps it alive:
  // guacd's per-connection process exits only when its last user detaches).
  onClose(key, clientConnection, isPrimary, err) {
    const session = this.sessions.get(key);
    if (!session) {
      return;
    }

    // A guacd error instruction is connection-scoped: the guacd connection
    // (and thus every member, and the joinability of session.guacId) is dead.
    // Drop the entry immediately so retrying clients get a fresh connection
    // instead of joining a corpse ("Connection does not exist").
    if (err && err.guacdError) {
      this.sessions.delete(key);
      session.rejectOpen(err); // no-op if the open promise already settled
      this.log(`session ${key}: dropped (${err.message})`);
      return;
    }

    if (this.opened.has(clientConnection)) {
      session.clients--;
      if (session.clients <= 0) {
        this.sessions.delete(key);
        this.log(`session ${key}: last client left, session dropped`);
      }
      return;
    }

    // A connection that never opened: if it was the primary, the session can
    // never open — fail any waiters and clear the slot for a fresh attempt.
    if (isPrimary && !session.guacId) {
      this.drop(session, err || new Error('connection closed before ready'));
    }
  }

  drop(session, err) {
    if (this.sessions.get(session.key) === session) {
      this.sessions.delete(session.key);
    }
    session.rejectOpen(err);
    this.log(`session ${session.key}: dropped before open (${err.message})`);
  }

  // has reports whether a session (live or pending) exists for ns/vmi.
  has(namespace, vmi) {
    return this.sessions.has(`${namespace}/${vmi}`);
  }

  size() {
    return this.sessions.size;
  }
}

module.exports = { SessionRegistry };
