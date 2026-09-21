import assert from 'node:assert/strict';
import { writeFileSync } from 'node:fs';
import { chromium } from 'playwright';

const config = JSON.parse(process.env.OPENRAILS_BROWSER_FIXTURE);
const pan = '4111111111111111'; // synthetic card, only entered in vendor fields
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext();
  await context.addInitScript(() => {
    window.captureCSP = [];
    document.addEventListener('securitypolicyviolation', event => {
      let blocked = event.blockedURI;
      try { const url=new URL(blocked); blocked=url.origin+url.pathname; } catch {}
      window.captureCSP.push({blocked,directive:event.effectiveDirective});
    });
  });
  const allowed = new Set([config.page, config.api, config.vendor_api, config.vendor_sdk].map(value => new URL(value).origin));
  const blocked = [];
  await context.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.protocol === 'http:' || url.protocol === 'https:') {
      if (!allowed.has(url.origin)) { blocked.push({origin:url.origin,path:url.pathname,type:route.request().resourceType()}); return route.abort(); }
    }
    return route.continue();
  });
  const page = await context.newPage();
  const coreRequests = [];
  const failures = [];
  const network = [];
  page.on("response", response => { const url=new URL(response.url()); if (!url.pathname.endsWith(".map")) network.push({ method:response.request().method(),status:response.status(),origin:url.origin,path:url.pathname }); });
  page.on('pageerror', error => failures.push(error.message));
  page.on('request', request => {
    if (request.url().startsWith(config.api + '/')) {
      assert.equal((request.postData() ?? '').includes(pan), false, 'PAN crossed OpenRails boundary');
      coreRequests.push(new URL(request.url()).pathname);
    }
  });
  await page.goto(config.page);
  await page.evaluate(async config => {
    const key = crypto.randomUUID();
    const request = async (method, path, body) => {
      const response = await fetch(config.api + path, { method, headers: { Authorization: `Bearer ${config.token}`, 'Content-Type': 'application/json', 'Idempotency-Key': key }, body: body === undefined ? undefined : JSON.stringify(body) });
      if (response.headers.get('Cache-Control') !== 'no-store') throw Error('Capture response is cacheable');
      const result = await response.json();
      if (!response.ok) throw Error(`Core capture refused ${response.status}/${result.error?.code}`);
      return result;
    };
    const setup = await request('POST', '/v1/me/checkout', { mode: 'payment_method', payment: { psp_id: config.psp_id, email: config.email, name_on_card: config.name } });
    const action = setup.capture;
    if (!action) throw Error('Core returned no capture action');
    window.captureSecretValues = [action.sdk_authorization,config.token];
    await new Promise((resolve, reject) => { const script = document.createElement('script'); script.src = action.sdk_url; script.onload = resolve; script.onerror = reject; document.head.append(script); });
    const hyper = window.Hyper(action.public_api_key, { isPreloadEnabled:false });
    const session = hyper.initPaymentMethodSession({ sdkAuthorization: action.sdk_authorization, locale: "en", storePaymentMethod: config.store === "true" });
    const form = session.createCardForm();
    for (const [type, selector] of [['cardNumber','#number'],['cardExpiry','#expiry'],['cardCvc','#cvc']]) form.create(type, {}).mount(selector);
    document.querySelector('#save').textContent = config.store === "true" ? "Save card" : "Tokenize once";
    document.querySelector('#save').onclick = async () => {
      try {
        const result = await form.tokenize();
        if (result.error) throw Error(`Vendor tokenization refused ${result.error.code}`);
        const token = result.associated_payment_methods?.[0]?.payment_method_token;
        if (token?.type !== 'payment_method_session_token' || !token.data) throw Error('Vendor returned no associated session token');
        const payment = { capture: { custodian_id: action.custodian_id, session_id: action.session_id, token: token.data } };
        if (config.store !== "true") {
          try { await request('POST', `/v1/me/checkout/${setup.id}/confirm`, { payment }); throw Error('Core attached volatile token'); }
          catch (error) { if (!error.message.startsWith('Core capture refused 409/')) throw error; }
          window.captureProof = { status:'refused_without_storage_consent', vendorSession:action.session_id, vendorCustomer:action.customer_id };
          return;
        }
        const confirmed = await request('POST', `/v1/me/checkout/${setup.id}/confirm`, { payment });
        const replay = await request('POST', `/v1/me/checkout/${setup.id}/confirm`, { payment });
        const read = await request('GET', `/v1/me/checkout/${setup.id}`);
        window.captureProof = { vendorSession:action.session_id, vendorCustomer:action.customer_id, status: confirmed.status, method: confirmed.payment_method_id, replayMethod: replay.payment_method_id, secretCleared: !confirmed.capture && !replay.capture && !read.capture };
      } catch (error) { window.captureFailure = error.message; }
    };
  }, config);
  await page.frameLocator('#number iframe').locator('input').fill(pan);
  await page.frameLocator('#expiry iframe').locator('input').fill('1230');
  await page.frameLocator('#cvc iframe').locator('input').fill('123');
  await page.getByRole('button', { name: config.store === 'true' ? 'Save card' : 'Tokenize once' }).click();
  await page.waitForFunction(() => window.captureProof || window.captureFailure);
  const result = await page.evaluate(() => ({ proof: window.captureProof, failure: window.captureFailure }));
  if (result.failure) console.log(JSON.stringify({captureFailure:result.failure,network,blocked}));
  assert.equal(result.failure, undefined);
  if (config.store === 'true') {
  assert.equal(result.proof.status, 'succeeded');
  assert.match(result.proof.method, /^pm_/);
  assert.equal(result.proof.method, result.proof.replayMethod);
  assert.equal(result.proof.secretCleared, true);
  } else assert.equal(result.proof.status, "refused_without_storage_consent");
  assert.deepEqual(failures, []);
  assert.deepEqual(blocked, [], "real CSP must stop every unused external resource before network dispatch");
  if (process.env.OPENRAILS_BROWSER_PRIVATE) writeFileSync(process.env.OPENRAILS_BROWSER_PRIVATE, JSON.stringify(await page.evaluate(() => window.captureSecretValues)), {mode:0o600});
  const cspBlocked = [];
  for (const frame of page.frames()) cspBlocked.push(...await frame.evaluate(() => window.captureCSP ?? []));
  console.log(JSON.stringify({ cspBlocked, externalHTTPRequests:0, vendorBrowserCapture: 'pass', vendorSession:result.proof.vendorSession, vendorCustomer:result.proof.vendorCustomer, persistent:config.store==='true', corePANRequests: 0, coreRequests, terminalReplay:config.store==='true', secretCleared:config.store==='true' }));
  await context.close();
} finally { await browser.close(); }
