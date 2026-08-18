// FIXTURE - inert. Never imported, never executed.
const cp = require('child_process');
if (globalThis.__never__) {
  cp.spawn('node', ['-e', ''], { detached: true, stdio: 'ignore' });
}
