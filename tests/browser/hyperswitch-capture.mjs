import assert from 'node:assert/strict';
import { writeFileSync } from 'node:fs';
import { chromium } from 'playwright';

const config = JSON.parse(process.env.OPENRAILS_BROWSER_FIXTURE);
const pan = '4111111111111111'; // synthetic card, only entered in vendor fields
const browser = await chromium.launch({ headless: true });
try {
  if (config.phase === 'membership') {
    const context = await browser.newContext();
    const allowed = new Set([config.page,config.api].map(value=>new URL(value).origin));
    const blocked=[];
    await context.route('**/*',route=>{
      const url=new URL(route.request().url());
      if (!allowed.has(url.origin)) {blocked.push(url.origin);return route.abort();}
      return route.continue();
    });
    const page=await context.newPage();
    await page.goto(config.page);
    await page.evaluate(async config=>{
      const request=async(method,path,body,key)=>{
        const response=await fetch(config.api+path,{method,headers:{Authorization:`Bearer ${config.token}`,'Content-Type':'application/json',...(key?{'Idempotency-Key':key}:{})},body:body===undefined?undefined:JSON.stringify(body)});
        const value=await response.json();
        if(!response.ok)throw Error(`Membership ${response.status}: ${JSON.stringify(value)}`);
        return value;
      };
      const key=crypto.randomUUID();
      const input={mode:'subscription',price_id:config.price_id,payment:{rail:'nmi',psp_id:config.psp_id,payment_method_id:config.method}};
      const quote=await request('POST','/v1/me/checkout',input,key);
      const repeated=await request('POST','/v1/me/checkout',input,key);
      if(quote.id!==repeated.id||quote.status!=='requires_action'||quote.subscription_id||quote.payment_id||!quote.membership_quote)throw Error('Quote is not an unaccepted stable agreement');
      window.membershipQuote=quote;
      const terms=document.createElement('p');
      const micros=BigInt(quote.amount);
      const displayed=`${micros/1000000n}.${(micros%1000000n).toString().padStart(6,'0').replace(/0+$/,'').padEnd(2,'0')}`;
      terms.textContent=`${quote.membership_quote.product_name}: ${displayed} ${quote.currency} every ${quote.membership_quote.cycle_hours} hours`;
      document.body.append(terms);
      const button=document.createElement('button');button.textContent='Subscribe';document.body.append(button);
      button.onclick=async()=>{
        try { window.membershipResult=await request('POST',`/v1/me/checkout/${quote.id}/confirm`,{payment:{rail:'nmi'}});window.membershipClicks=(window.membershipClicks??0)+1; }
        catch(error){window.membershipFailure=error.message;}
      };
    },config);
    const quote=await page.evaluate(()=>window.membershipQuote);
    assert.equal(quote.amount,'9990000');
    assert.equal(quote.currency,'USD');
    assert.equal(quote.membership_quote.cycle_hours,720);
    assert.equal(await page.getByText('Browser membership: 9.99 USD every 720 hours',{exact:true}).isVisible(),true);
    const checkpoint=await fetch(config.checkpoint,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({price_id:config.price_id,session_id:quote.id})});
    assert.equal(checkpoint.status,200);
    const before=await checkpoint.json();
    assert.equal(before.financial,0);
    assert.equal(before.provider_calls,Number(config.before_provider_calls));
    await page.getByRole('button',{name:'Subscribe',exact:true}).click();
    await page.waitForFunction(()=>window.membershipResult||window.membershipFailure);
    const first=await page.evaluate(()=>({result:window.membershipResult,failure:window.membershipFailure}));
    assert.equal(first.failure,undefined);
    assert.equal(first.result.status,'succeeded');
    assert.match(first.result.subscription_id,/^sub_/);
    assert.match(first.result.payment_id,/^pay_/);
    const expired=await fetch(config.expire,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({session_id:quote.id})});
    assert.equal(expired.status,204);
    await page.getByRole('button',{name:'Subscribe',exact:true}).click();
    await page.waitForFunction(()=>window.membershipClicks===2||window.membershipFailure);
    const replay=await page.evaluate(()=>window.membershipResult);
    assert.equal(replay.subscription_id,first.result.subscription_id);
    assert.equal(replay.payment_id,first.result.payment_id);
    assert.deepEqual(blocked,[]);
    console.log(JSON.stringify({membership:first.result,quote,externalHTTPRequests:0}));
    await context.close();
  } else {
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
  let invoicePay;
  if (config.invoice_fixture) {
    const seeded = await fetch(config.invoice_fixture,{method:'POST'});
    assert.equal(seeded.status,200);
    config.invoice_id=(await seeded.json()).invoice_id;
    await page.evaluate(config => {
      const button = document.createElement('button'); button.textContent='Pay invoice'; document.body.append(button);
      const key = crypto.randomUUID();
      button.onclick=async()=>{
        try {
          const response=await fetch(config.api+`/v1/me/invoices/${config.invoice_id}/pay-now`,{method:'POST',headers:{Authorization:`Bearer ${config.token}`,'Content-Type':'application/json','Idempotency-Key':key},body:JSON.stringify({payment_method_id:window.captureProof.method})});
          const body=await response.json();
          if(!response.ok)throw Error(`Invoice payment refused ${response.status}/${body.error?.code}`);
          window.invoiceProof=body;
          window.invoiceKey=key;
        } catch(error){window.invoiceFailure=error.message;}
      };
    },config);
    await page.getByRole('button',{name:'Pay invoice',exact:true}).click();
    await page.waitForFunction(()=>window.invoiceProof||window.invoiceFailure);
    const payment=await page.evaluate(()=>({result:window.invoiceProof,failure:window.invoiceFailure}));
    assert.equal(payment.failure,undefined);
    assert.equal(payment.result.operation.status,'succeeded');
    assert.equal(payment.result.invoice.status,'paid');
    invoicePay=payment.result;
    await page.getByRole('button',{name:'Pay invoice',exact:true}).click();
    await page.waitForFunction(()=>window.invoiceProof?.replayed===true);
    const replay=await page.evaluate(()=>window.invoiceProof);
    assert.equal(replay.operation.id,invoicePay.operation.id);
    assert.equal(replay.invoice.status,'paid');
  }
  assert.deepEqual(failures, []);
  assert.deepEqual(blocked, [], "real CSP must stop every unused external resource before network dispatch");
  if (process.env.OPENRAILS_BROWSER_PRIVATE) writeFileSync(process.env.OPENRAILS_BROWSER_PRIVATE, JSON.stringify(await page.evaluate(() => window.captureSecretValues)), {mode:0o600});
  const cspBlocked = [];
  for (const frame of page.frames()) cspBlocked.push(...await frame.evaluate(() => window.captureCSP ?? []));
  console.log(JSON.stringify({ cspBlocked, externalHTTPRequests:0, vendorBrowserCapture: 'pass', vendorSession:result.proof.vendorSession, vendorCustomer:result.proof.vendorCustomer, method:result.proof.method, invoicePay, invoiceKey:await page.evaluate(()=>window.invoiceKey), persistent:config.store==='true', corePANRequests: 0, coreRequests, terminalReplay:config.store==='true', secretCleared:config.store==='true' }));
  await context.close();
  }
} finally { await browser.close(); }
