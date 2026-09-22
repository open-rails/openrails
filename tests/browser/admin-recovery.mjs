import assert from 'node:assert/strict';
import { chromium } from 'playwright';

const config = JSON.parse(process.env.OPENRAILS_BROWSER_FIXTURE);
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  const failures = [];
  let confirmations = 0;
  page.on('pageerror', error => failures.push(error.message));
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/account/recovery/confirm')) confirmations++;
  });
  await page.goto(`${config.url}/admin/login`);
  await page.getByLabel('Email or username').fill(config.email);
  await page.getByLabel('Password', { exact: true }).fill(config.password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await page.getByRole('heading', { name: 'Restore your account?' }).waitFor();
  assert.equal(confirmations, 0);
  assert.match(await page.getByText('You can recover this account until').textContent(), /until/);
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await page.getByRole('heading', { name: 'Sign in to your console' }).waitFor();
  assert.equal(confirmations, 0);

  // Exercise the browser callback fragment with a real server-issued proof.
  // Cancellation above must have left the account deleted.
  const response = await context.request.post(`${config.url}/auth/password/login`, { data: { identifier: config.email, password: config.password } });
  assert.equal(response.status(), 409);
  const { error } = await response.json();
  assert.equal(error.code, 'account_recovery_required');
  const fragment = new URLSearchParams({ error: error.code, recovery: JSON.stringify(error.metadata.recovery), access_token: 'must-not-adopt-error-fragment-token' });
  await page.goto(`${config.url}/admin/#${fragment}`);
  await page.getByRole('heading', { name: 'Restore your account?' }).waitFor();
  assert.equal(await page.evaluate(() => location.hash), '');
  assert.equal(await page.evaluate(() => sessionStorage.getItem('openrails.admin.tokens')), null);
  assert.equal(await page.evaluate(() => localStorage.getItem('openrails.admin.tokens')), null);
  if (process.env.OPENRAILS_BROWSER_SCREENSHOT) await page.screenshot({ path: process.env.OPENRAILS_BROWSER_SCREENSHOT, fullPage: true });
  assert.equal(confirmations, 0);
  await page.getByRole('button', { name: 'Restore account', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Your account has been restored. Sign in to continue.' }).waitFor();
  assert.equal(confirmations, 1);
  assert.equal(await page.getByLabel('Password', { exact: true }).inputValue(), '');
  assert.equal(await page.evaluate(() => sessionStorage.getItem('openrails.admin.tokens')), null);
  assert.equal(await page.getByRole('button', { name: 'Restore account', exact: true }).count(), 0);
  assert.deepEqual(failures, []);
  console.log('Compiled console + real Auth124: password proof, explicit cancel, callback fragment cleanup, one confirmation and fresh sign-in requirement passed.');
  await context.close();
} finally {
  await browser.close();
}
