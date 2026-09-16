import assert from 'node:assert/strict';
import { chromium } from 'playwright';

const config = JSON.parse(process.env.OPENRAILS_BROWSER_FIXTURE);
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext();
  await context.addInitScript(({ token, merchant }) => {
    sessionStorage.setItem('openrails.admin.tokens', JSON.stringify({ access_token: token, merchant }));
  }, config);
  const page = await context.newPage();
  const failures = [];
  page.on('pageerror', error => failures.push(error.message));
  await page.goto(`${config.url}settings?tab=notifications`);
  await page.getByRole('heading', { name: 'Email delivery' }).waitFor();
  await page.getByRole('heading', { name: 'Webhooks', exact: true }).waitFor();
  assert.equal(await page.getByRole('tab', { name: 'Alerts', exact: true }).count(), 0);
  assert.equal(await page.getByText('Alert rules', { exact: true }).count(), 0);
  await page.getByRole('button', { name: 'Add webhook', exact: true }).click();
  await page.getByLabel('Name', { exact: true }).fill('Browser notification sink');
  await page.getByLabel('Webhook URL').fill('https://example.com/notifications');
  await page.getByRole('dialog').getByRole('button', { name: 'Add webhook', exact: true }).click();
  await page.getByRole('dialog').waitFor({ state: 'hidden' });
  await page.getByRole('cell', { name: 'Browser notification sink', exact: true }).waitFor();
  assert.equal(await page.getByRole('cell', { name: 'example.com', exact: true }).count(), 1);
  if (process.env.OPENRAILS_BROWSER_SCREENSHOT) await page.screenshot({ path: process.env.OPENRAILS_BROWSER_SCREENSHOT, fullPage: true });
  assert.deepEqual(failures, []);
  console.log('Notifications navigation, rule-editor absence and encrypted webhook creation passed against compiled admin + real API.');
  await context.close();
} finally {
  await browser.close();
}
