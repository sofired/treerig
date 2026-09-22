import net from 'node:net';

/**
 * A loopback port nothing is listening on right now, probed upward from a
 * base outside the kernel's ephemeral range so another process is less likely
 * to claim it between this check and the bind. The base is spread by process
 * id, so two runs started at once on one machine rarely begin at the same
 * port. The remaining race is accepted: its symptom is a loud failure to
 * start, never a wrong result.
 */
export async function freePort(from = 18_000 + (process.pid % 500)): Promise<number> {
  const last = from + 199;
  for (let port = from; port <= last; port += 1) {
    if (await isFree(port)) return port;
  }
  throw new Error(`No free loopback port between ${from} and ${last}.`);
}

function isFree(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.unref();
    server.on('error', () => resolve(false));
    server.listen(port, '127.0.0.1', () => {
      server.close(() => resolve(true));
    });
  });
}
