'use strict';

const crypto = require('crypto');

// TokenFactory builds the AES-256-CBC tokens guacamole-lite expects in the
// ?token= query parameter. The key is random per process: tokens are minted
// and consumed entirely inside this server (clients authenticate with a JWT
// and never see tokens), so nothing needs to survive a restart.
class TokenFactory {
  constructor() {
    this.cypher = 'AES-256-CBC';
    this.key = crypto.randomBytes(32);
  }

  // cryptOptions plugs into guacamole-lite's clientOptions.crypt.
  cryptOptions() {
    return { cypher: this.cypher, key: this.key };
  }

  // encrypt produces base64(JSON({iv, value})) as documented by
  // guacamole-lite's README.
  encrypt(value) {
    const iv = crypto.randomBytes(16);
    const cipher = crypto.createCipheriv(this.cypher, this.key, iv);
    let encrypted = cipher.update(JSON.stringify(value), 'utf8', 'base64');
    encrypted += cipher.final('base64');
    const data = { iv: iv.toString('base64'), value: encrypted };
    return Buffer.from(JSON.stringify(data)).toString('base64');
  }

  // decrypt is the inverse of encrypt (used by tests).
  decrypt(token) {
    const data = JSON.parse(Buffer.from(token, 'base64').toString('utf8'));
    const iv = Buffer.from(data.iv, 'base64');
    const decipher = crypto.createDecipheriv(this.cypher, this.key, iv);
    let decrypted = decipher.update(data.value, 'base64', 'utf8');
    decrypted += decipher.final('utf8');
    return JSON.parse(decrypted);
  }
}

module.exports = { TokenFactory };
