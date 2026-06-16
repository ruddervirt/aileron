'use strict';

// dns1123 mirrors what Kubernetes enforces for resource names. Path segments
// land verbatim in VMI lookups; accepting anything else could smuggle `..`,
// `?`, `#`, or other control characters. Parity with the Go proxies'
// validName (stabilizer/internal/vncbridge/bridge.go).
const DNS1123 = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

function validName(s) {
  return typeof s === 'string' && s.length > 0 && s.length <= 253 && DNS1123.test(s);
}

// parseTwoSegments extracts and validates "{a}/{b}" after the given prefix.
// Returns [a, b] or null.
function parseTwoSegments(pathname, prefix) {
  if (!pathname.startsWith(prefix)) {
    return null;
  }
  const rest = pathname.slice(prefix.length).replace(/\/+$/, '');
  const parts = rest.split('/');
  if (parts.length !== 2 || !validName(parts[0]) || !validName(parts[1])) {
    return null;
  }
  return parts;
}

module.exports = { validName, parseTwoSegments };
