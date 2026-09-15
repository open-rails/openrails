import assert from 'node:assert/strict';
import { chromium } from 'playwright';
const config = JSON.parse(process.env.OPENRAILS_BROWSER_FIXTURE);
const browser = await chromium.launch({ headless: true, args: ['--no-proxy-server', '--host-resolver-rules=MAP merchant.test 127.0.0.1,MAP sibling.merchant.test 127.0.0.1,MAP attacker.test 127.0.0.1'] });
try {
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage();
  await page.goto(config.issuer);
  const delegation = await page.evaluate(async (cfg) => {
    const b64 = bytes => btoa(String.fromCharCode(...new Uint8Array(bytes))).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
    const encode = value => b64(new TextEncoder().encode(JSON.stringify(value)));
    const login = await fetch('/auth/password/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ identifier: cfg.email, password: cfg.password }) });
    if (!login.ok) throw new Error(`login ${login.status}`);
    const { access_token } = await login.json();
    const key = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    const jwk = await crypto.subtle.exportKey('jwk', key.publicKey);
    const proof = async (method, target, token) => {
      const header = encode({ typ: 'dpop+jwt', alg: 'ES256', jwk: { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y } });
      const payload = encode({ htm: method, htu: target, ath: b64(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token))), iat: Math.floor(Date.now()/1000), jti: crypto.randomUUID() });
      const input = `${header}.${payload}`;
      const signature = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key.privateKey, new TextEncoder().encode(input));
      return `${input}.${b64(signature)}`;
    };
    const target = `${cfg.issuer}/auth/delegated/token`;
    const mint = await fetch(target, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${access_token}`, DPoP: await proof('POST', target, access_token) }, body: JSON.stringify({ audiences: ['openrails'], requested_grant: {} }) });
    if (!mint.ok) throw new Error(`delegated mint ${mint.status}: ${await mint.text()}`);
    const { token, token_type } = await mint.json();
    const resource = `${cfg.resource}/v1/me/status`;
    const senderProof = await proof('GET', resource, token);
    const headers = { Authorization: `DPoP ${token}`, DPoP: senderProof };
    const first = await fetch(resource, { headers, credentials: 'omit', redirect: 'error' });
    const replay = await fetch(resource, { headers, credentials: 'omit' });
    const downgrade = await fetch(resource, { headers: { Authorization: `Bearer ${token}`, DPoP: await proof('GET', resource, token) }, credentials: 'omit' });
    const missing = await fetch(resource, { headers: { Authorization: `DPoP ${token}` }, credentials: 'omit' });
    const fresh = await fetch(resource, { headers: { Authorization: `DPoP ${token}`, DPoP: await proof('GET', resource, token) }, credentials: 'omit' });
    return { first: first.status, replay: replay.status, downgrade: downgrade.status, missing: missing.status, fresh: fresh.status, token_type, extractable: key.privateKey.extractable, challenge: replay.headers.get('WWW-Authenticate') };
  }, config);
  assert.deepEqual(delegation, { first: 200, replay: 401, downgrade: 401, missing: 401, fresh: 200, token_type: 'DPoP', extractable: false, challenge: 'DPoP error="invalid_dpop_proof", algs="ES256"' });

  await context.addCookies([{ name: 'session', value: 'approved', domain: '.merchant.test', path: '/', secure: true, httpOnly: true, sameSite: 'None' }]);
  for (const origin of [config.cookie, config.sibling, config.attacker]) {
    await page.goto(origin);
    for (const body of [undefined, '{}']) {
      const status = await page.evaluate(async ({ target, body }) => {
        try { return (await fetch(target, { method: 'POST', credentials: 'include', headers: { 'Content-Type': 'text/plain' }, body })).status; } catch { return null; }
      }, { target: `${config.cookie}/action`, body });
      assert.equal(status, origin === config.cookie ? 204 : null, `${origin} body=${body}`);
    }
  }
  await page.goto(config.cookie);
  const receipt = await page.evaluate(async () => (await fetch('/receipt')).json());
  assert.deepEqual(receipt, { mutations: 2, attached: 6, denied: 4 });
  console.log(JSON.stringify({ delegation, cookie: 'same-origin accepted; same-site sibling and cross-site attached-cookie POSTs refused; JSON and bodyless checked', mutations: receipt.mutations }));
  await context.close();
} finally { await browser.close(); }
