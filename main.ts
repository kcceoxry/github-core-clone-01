// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind a port,
// tell the env the port, then poll until the env says the trial is over and
// exit 0. The server itself is the Go binary beside this file
// (forge-server, built from cmd/server); this file only launches it, reads
// the port it bound from its stdout, and drives the register/done loop.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial
//      container). Without it there is nothing to register with, so the
//      server is left running standalone on its port instead of exiting:
//      that is how the server is run locally for grading practice.
//   2. Spawn ./forge-server with --port 0 and wait for its
//      "[forge] listening on port N" line. Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The first register wins;
//      a 409 means another entry already registered.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. A failed
//      or slow poll is retried with a short exponential backoff; the process
//      never exits before the env says done.
//   5. Kill the server and exit 0.
//
// main.ts imports Node built-ins only: no package, no model call.

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** How long to wait for the Go binary to print its listening port. */
const SERVER_START_TIMEOUT_MS = 15_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/** The Go binary beside this file, resolved from this file's own location. */
function serverBinaryPath(): string {
  return path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge-server');
}

/**
 * Spawn the Go server and wait for its "[forge] listening on port N" line.
 * The child's stderr is inherited so a bind failure is visible; its stdout
 * is consumed here for the port line only.
 */
function startServer(): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const binary = serverBinaryPath();
    const child = spawn(binary, ['--port', '0'], { stdio: ['ignore', 'pipe', 'inherit'] });
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill('SIGKILL');
      reject(new Error(`the Go server did not print its listening port within ${SERVER_START_TIMEOUT_MS} ms`));
    }, SERVER_START_TIMEOUT_MS);
    let buffer = '';
    child.stdout?.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const match = buffer.match(/\[forge\] listening on port (\d+)/);
      if (match && !settled) {
        settled = true;
        clearTimeout(timer);
        resolve({ child, port: Number(match[1]) });
      }
    });
    child.once('error', (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(`could not start ${binary}: ${err.message}`));
    });
    child.once('exit', (code) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(`the Go server exited early with code ${code}`));
    });
  });
}

/** POST the bound port until the env accepts it. 200 is normal; 409 means a first register already won. */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/** Start the server, register it, wait for done, then stop the server and exit 0. */
async function main(): Promise<void> {
  const { child, port } = await startServer();
  process.stdout.write(`[entry] listening on port ${port}\n`);
  if (ENV_URL === '') {
    process.stdout.write('[entry] MAKEFUN_ENV_URL is not set: serving standalone (no env to register with)\n');
    await new Promise<void>((resolve) => {
      child.once('exit', () => resolve());
      process.once('SIGINT', () => resolve());
      process.once('SIGTERM', () => resolve());
    });
    return;
  }
  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');
  await waitForDone();
  child.kill('SIGTERM');
  await new Promise<void>((resolve) => {
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      resolve();
    }, 5_000);
    child.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
  });
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
