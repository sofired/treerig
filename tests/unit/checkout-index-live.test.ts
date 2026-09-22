import { runInNewContext } from 'node:vm';
import { describe, expect, it } from 'vitest';
import { liveUpdateScript, renderIndexHtml } from '../../src/checkout-index/render.ts';
import type { IndexSnapshot } from '../../src/checkout-index/snapshot.ts';

// The page's own script, run as the browser gets it — as text, in a context
// that knows nothing but the handful of DOM calls it makes. What is pinned here
// is the scheduling contract: the loop must re-arm after every outcome, because
// a page that quietly stops updating looks exactly like one with nothing to say.

interface FakeElement {
  tag: string;
  /** What the browser would measure; the region is focusable only when they differ. */
  scrollWidth: number;
  clientWidth: number;
  removeAttribute(name: string): void;
  textContent: string;
  /** How many times the text was assigned — a live region re-announces on every write. */
  writes: number;
  innerHTML: string;
  type: string;
  className: string;
  attributes: Map<string, string>;
  children: FakeElement[];
  handlers: Map<string, () => void>;
  getAttribute(name: string): string | null;
  setAttribute(name: string, value: string): void;
  contains(other: unknown): boolean;
  matches(selector: string): boolean;
  addEventListener(event: string, handler: () => void): void;
  appendChild(child: FakeElement): void;
}

function element(
  attributes: Record<string, string> = {},
  options: { focusVisible?: boolean; knowsFocusVisible?: boolean; tag?: string } = {}
): FakeElement {
  let text = '';
  const self: FakeElement = {
    tag: options.tag ?? 'div',
    get textContent() { return text; },
    set textContent(value: string) {
      text = value;
      self.writes += 1;
    },
    writes: 0,
    innerHTML: '',
    type: '',
    className: '',
    attributes: new Map(Object.entries(attributes)),
    children: [],
    handlers: new Map(),
    scrollWidth: 0,
    clientWidth: 0,
    getAttribute: (name) => self.attributes.get(name) ?? null,
    setAttribute: (name, value) => void self.attributes.set(name, value),
    removeAttribute: (name) => void self.attributes.delete(name),
    contains: (other) => self.children.includes(other as FakeElement),
    matches: (selector) => {
      // What a browser does with a pseudo-class it has never heard of. The
      // loop has to survive it, so the double has to be able to do it.
      if (options.knowsFocusVisible === false) throw new SyntaxError(`'${selector}' is not a valid selector`);
      return selector === ':focus-visible' && options.focusVisible === true;
    },
    addEventListener: (event, handler) => void self.handlers.set(event, handler),
    appendChild: (child) => void self.children.push(child)
  };
  return self;
}

/** One attribute out of a rendered page, the way a browser would have parsed it. */
function attribute(html: string, name: string): string | undefined {
  return new RegExp(`${name}="([^"]+)"`).exec(html)?.[1];
}

/** Lets the script's fetch chain run to its end; the sandbox's own clock is a double. */
function settle(): Promise<void> {
  return new Promise((resolve) => void setTimeout(resolve, 0));
}

const SNAPSHOT: IndexSnapshot = {
  generatedAt: '2026-09-21T12:00:00.000Z',
  dockerReachable: true,
  entries: [{
    repositoryId: 'r', worktreeId: 'w', project: 'acme', domain: 'acme.localhost', slug: 'feature-a',
    host: 'feature-a.acme.localhost', branch: 'feature/a', root: '/work/feature-a',
    status: 'running', services: []
  }]
};

/** The same page the tool writes, rendered at a later moment. */
function laterPage(generatedAt: string, entries = SNAPSHOT.entries): string {
  return renderIndexHtml({ ...SNAPSHOT, generatedAt, entries });
}

/** A second checkout, so a later page differs in its rows and its title. */
const MORE = [...SNAPSHOT.entries, {
  repositoryId: 'r', worktreeId: 'w2', project: 'acme', domain: 'acme.localhost', slug: 'feature-b',
  host: 'feature-b.acme.localhost', branch: 'feature/b', root: '/work/feature-b',
  status: 'stopped' as const, services: []
}];

const CHANGED = () => Promise.resolve({ ok: true, text: () => Promise.resolve(laterPage('2026-09-21T12:30:00.000Z', MORE)) });

interface World {
  run: () => void;
  fire: (event: string) => void;
  timers: { delay: number; run: () => void }[];
  armed: () => number;
  advance: () => Promise<void>;
  /** Run only what was scheduled for right now, leaving the poll timer armed. */
  flush: () => Promise<void>;
  stamp: FakeElement;
  main: FakeElement;
  header: FakeElement;
  announcer: FakeElement;
  warning: FakeElement;
  wrap: FakeElement;
  document: { title: string; hidden: boolean; activeElement: FakeElement | null };
  requests: { url: string; init: Record<string, unknown> }[];
  timeouts: number[];
  reloads: number;
  fetches: number;
}

interface WorldOptions {
  respond?: () => Promise<{ ok: boolean; text: () => Promise<string> }>;
  focusVisible?: boolean;
  knowsFocusVisible?: boolean;
  shell?: string;
  body?: string;
  protocol?: string;
  abortSignal?: boolean;
  /** A browser that refuses to store anything: private mode, or an opaque origin. */
  storageThrows?: boolean;
  /** A reload the browser has not acted on yet, so the old page is still running. */
  reloadIsNoop?: boolean;
  /** Throw where a browser would throw synchronously, rather than rejecting. */
  throwOnFetch?: boolean;
}

function world(options: WorldOptions = {}): World {
  const rendered = renderIndexHtml(SNAPSHOT);
  const shell = options.shell ?? attribute(rendered, 'data-shell') ?? '';
  const body = options.body ?? attribute(rendered, 'data-body') ?? '';
  const stamp = element({ datetime: SNAPSHOT.generatedAt });
  const main = element({ 'data-body': body }, { knowsFocusVisible: options.knowsFocusVisible ?? true });
  const header = element();
  const announcer = element();
  const warning = element();
  const wrap = element();
  interface Timer { delay: number; run: () => void }
  // A reload destroys the old document's JavaScript. Each page load gets its
  // own timer list, and only the newest is ever advanced, so a loop left behind
  // by a reloaded page cannot keep polling beside its replacement.
  let timers: Timer[] = [];
  const requests: { url: string; init: Record<string, unknown> }[] = [];
  const timeouts: number[] = [];
  const stored = new Map<string, string>();
  const resizes = new Map<string, () => void>();
  /** The page the server last served: what a reload brings back. */
  let served = rendered;
  const documentState = { title: 'stale title', hidden: false, activeElement: null as FakeElement | null };
  const counters = { reloads: 0, fetches: 0 };

  const documentDouble = {
    get title() { return documentState.title; },
    set title(value: string) { documentState.title = value; },
    get hidden() { return documentState.hidden; },
    get activeElement() { return documentState.activeElement; },
    documentElement: element({ 'data-shell': shell }, { knowsFocusVisible: options.knowsFocusVisible ?? true }),
    querySelector: (selector: string) => selector === 'header time' ? stamp : selector === 'header' ? header : selector === 'main' ? main : null,
    // One scroll region, whose width the page decides; the script makes it
    // focusable only while it has something off-screen to scroll to.
    querySelectorAll: (selector: string) => selector === '.table-wrap' ? [wrap] : [],
    getElementById: (id: string) => id === 'index-announcer' ? announcer : id === 'index-stale' ? warning : null,
    createElement: (tag: string) => element({}, { tag }),
    addEventListener: (event: string, handler: () => void) => void header.handlers.set(event, handler)
  };

  function load(): void {
    const own: Timer[] = [];
    timers = own;
    const sandbox: Record<string, unknown> = {
      document: documentDouble,
      // The page re-checks whether a table needs a tab stop when the window changes.
      window: { addEventListener: (event: string, handler: () => void) => void resizes.set(event, handler) },
      location: {
        pathname: '/',
        protocol: options.protocol ?? 'http:',
        // Whatever the server last served is what comes back.
        reload: () => {
          counters.reloads += 1;
          if (options.reloadIsNoop === true) return;
          // A new document, carrying what was served: the reader sees that
          // page, and the script starts again against it.
          header.children.length = 0;
          documentDouble.documentElement.setAttribute('data-shell', attribute(served, 'data-shell') ?? shell);
          stamp.setAttribute('datetime', attribute(served, 'datetime') ?? SNAPSHOT.generatedAt);
          main.setAttribute('data-body', attribute(served, 'data-body') ?? body);
          main.innerHTML = served.slice(served.indexOf('>', served.indexOf('<main')) + 1, served.indexOf('</main>'));
          load();
        }
      },
      sessionStorage: {
        getItem: (key: string) => {
          if (options.storageThrows === true) throw new Error('storage is disabled');
          return stored.get(key) ?? null;
        },
        setItem: (key: string, value: string) => {
          if (options.storageThrows === true) throw new Error('storage is disabled');
          stored.set(key, value);
        }
      },
      setTimeout: (run: () => void, delay: number) => {
        own.push({ delay, run });
        return own.length;
      },
      clearTimeout: (handle: number) => {
        const timer = own[handle - 1];
        if (typeof handle === 'number' && timer) timer.delay = -1;
      },
      fetch: (url: string, init: Record<string, unknown>) => {
        if (options.throwOnFetch === true) throw new Error('fetch is not a function here');
        counters.fetches += 1;
        requests.push({ url, init });
        const answer = (options.respond ?? (() => Promise.resolve({ ok: true, text: () => Promise.resolve(laterPage('2026-09-21T12:30:00.000Z')) })))();
        // Remember what was served, so a reload can bring the page back as that.
        return answer.then((response) => ({
          ok: response.ok,
          text: () => response.text().then((html) => {
            served = html;
            return html;
          })
        }));
      },
      Promise,
      Error,
      SyntaxError,
      Date,
      isNaN,
      String
    };
    // Absent on an engine old enough not to have it: the request is then built
    // without a timeout rather than not at all.
    if (options.abortSignal !== false) {
      sandbox['AbortSignal'] = { timeout: (ms: number) => { timeouts.push(ms); return { ms }; } };
    }
    runInNewContext(liveUpdateScript(), sandbox);
  }

  return {
    run: load,
    fire: (event) => (header.handlers.get(event) ?? resizes.get(event))?.(),
    get timers() { return timers; },
    armed: () => timers.filter((timer) => timer.delay >= 0).length,
    advance: async () => {
      // Append-only: a handle is an index, so the array must never be
      // renumbered under a `clearTimeout` the page has not made yet.
      const pending = timers.filter((timer) => timer.delay >= 0);
      for (const timer of pending) timer.delay = -1;
      for (const timer of pending) timer.run();
      await settle();
    },
    flush: async () => {
      const pending = timers.filter((timer) => timer.delay === 0);
      for (const timer of pending) timer.delay = -1;
      for (const timer of pending) timer.run();
      await settle();
    },
    stamp,
    main,
    header,
    announcer,
    warning,
    wrap,
    document: documentState,
    requests,
    timeouts,
    get reloads() { return counters.reloads; },
    get fetches() { return counters.fetches; }
  };
}

describe('the page\'s live-update script', () => {
  it('renders the stamp in local time and arms a timer as soon as it runs', () => {
    const live = world();
    live.run();
    expect(live.stamp.textContent).toBe(new Date(SNAPSHOT.generatedAt).toLocaleString());
    expect(live.armed()).toBe(1);
    expect(live.header.children.map((child) => child.textContent)).toEqual(['Pause updates']);
  });

  it('swaps the table, the title, and the announcement when the page it fetches is newer', async () => {
    const live = world({ respond: CHANGED });
    live.run();
    await live.advance();
    expect(live.main.innerHTML).toContain('feature/b');
    expect(live.main.innerHTML).not.toContain('<script');
    expect(live.stamp.getAttribute('datetime')).toBe('2026-09-21T12:30:00.000Z');
    expect(live.document.title).toBe('Checkouts (2)');
    expect(live.announcer.textContent).toBe('Checkouts (2) updated.');
    expect(live.armed()).toBe(1);
    // Conditional, non-redirecting, and bounded: a hung router must not wedge the loop.
    expect(live.requests).toEqual([{ url: '/', init: { cache: 'no-cache', redirect: 'error', signal: { ms: 10000 } } }]);
    expect(live.timeouts).toEqual([10000]);
  });

  it('advances the clock quietly when a regeneration changed nothing on the page', async () => {
    // The first poll after a load is the common case: some other checkout's
    // `up` regenerated the page, and these rows are byte for byte what the tab
    // already shows.
    let stamped = '2026-09-21T13:00:00.000Z';
    const live = world({ respond: () => Promise.resolve({ ok: true, text: () => Promise.resolve(laterPage(stamped)) }) });
    live.run();
    await live.advance();
    expect(live.main.innerHTML).toBe('');
    expect(live.announcer.textContent).toBe('');
    expect(live.stamp.getAttribute('datetime')).toBe(stamped);

    stamped = '2026-09-21T14:00:00.000Z';
    await live.advance();
    expect(live.main.innerHTML).toBe('');
    expect(live.stamp.getAttribute('datetime')).toBe(stamped);
  });

  it('skips the fetch but keeps the timer armed while the tab is hidden', async () => {
    const live = world();
    live.run();
    live.document.hidden = true;
    await live.advance();
    expect(live.fetches).toBe(0);
    expect(live.armed()).toBe(1);
  });

  it('holds new rows while a keyboard reader is inside the table, and offers them again after', async () => {
    const focused = element({}, { focusVisible: true });
    const live = world({ respond: CHANGED });
    live.run();
    live.main.children.push(focused);
    live.document.activeElement = focused;
    await live.advance();

    // Read, not swapped: the reader keeps their place, and the clock does not
    // move either, so the next tick offers the same rows again.
    expect(live.fetches).toBe(1);
    expect(live.main.innerHTML).toBe('');
    expect(live.warning.textContent).toBe('New rows waiting.');
    expect(live.warning.className).toBe('waiting');
    expect(live.stamp.getAttribute('datetime')).toBe(SNAPSHOT.generatedAt);
    // Said once, however long they keep reading, and asked for once: focus
    // moves inside the table must not pull the page again.
    const written = live.warning.writes;
    const asked = live.fetches;
    live.fire('focusout');
    await live.advance();
    expect(live.warning.writes).toBe(written);
    expect(live.fetches).toBe(asked + 1);

    // Leaving the table puts them in at once, and without asking for the page
    // again: a focus move inside the table would otherwise fetch per keystroke.
    const read = live.fetches;
    live.document.activeElement = null;
    live.fire('focusout');
    await live.advance();
    expect(live.main.innerHTML).toContain('feature/b');
    expect(live.fetches).toBe(read + 1);
    expect(live.warning.textContent).toBe('');
    expect(live.stamp.getAttribute('datetime')).toBe('2026-09-21T12:30:00.000Z');
  });

  it('swaps under a click, which leaves focus on the link just followed', async () => {
    const clicked = element({}, { focusVisible: false });
    const live = world({ respond: CHANGED });
    live.run();
    live.main.children.push(clicked);
    live.document.activeElement = clicked;
    await live.advance();
    expect(live.fetches).toBe(1);
    expect(live.main.innerHTML).toContain('feature/b');
  });

  it('treats a page it cannot read as a failure, however healthy the response looked', async () => {
    // Something else bound to the port, a captive portal, a proxy notice: a
    // 200 that is not this page leaves the rows stale, and must say so rather
    // than resetting the count every fifteen seconds.
    const live = world({ respond: () => Promise.resolve({ ok: true, text: () => Promise.resolve('<html><body>hello</body></html>') }) });
    live.run();
    for (let attempt = 0; attempt < 4; attempt += 1) await live.advance();
    expect(live.warning.textContent).toBe('Not updating.');
    expect(live.main.innerHTML).toBe('');
    expect(live.stamp.getAttribute('datetime')).toBe(SNAPSHOT.generatedAt);
    expect(live.armed()).toBe(1);
  });

  it('reloads once for markup a newer berth built, and carries on from there', async () => {
    const live = world({ shell: 'a-different-build', respond: CHANGED });
    live.run();
    await live.advance();
    expect(live.reloads).toBe(1);
    // The reloaded document already carries the served page, so the tab is
    // showing those rows without a swap — and reads on rather than reloading.
    expect(live.main.innerHTML).toContain('feature/b');
    expect(live.stamp.getAttribute('datetime')).toBe('2026-09-21T12:30:00.000Z');
    await live.advance();
    expect(live.reloads).toBe(1);
    expect(live.fetches).toBe(2);
    expect(live.warning.textContent).toBe('');
  });

  it('reloads once per build it meets, not once per poll, when two berths alternate', async () => {
    // One worktree on an older berth, another on a newer one: each regenerates
    // the machine-wide page, so the shell flips back and forth.
    let flip = false;
    let minute = 30;
    const live = world({
      shell: 'build-a',
      respond: () => {
        flip = !flip;
        minute += 1;
        // Each regeneration moves the stamp, as a real one does.
        return Promise.resolve({
          ok: true,
          text: () => Promise.resolve(
            laterPage(`2026-09-21T12:${String(minute)}:00.000Z`, MORE)
              .replace(/data-shell="[^"]+"/, `data-shell="build-${flip ? 'b' : 'c'}"`)
          )
        });
      }
    });
    live.run();
    for (let attempt = 0; attempt < 6; attempt += 1) await live.advance();
    // One for each of the two builds, and never again: a shell it has already
    // reloaded for is one it says it cannot follow rather than reloading for.
    expect(live.reloads).toBe(2);
    expect(live.warning.writes).toBeGreaterThan(0);
  });

  it('stops polling the moment it asks for a reload, however long the browser takes', async () => {
    const live = world({ shell: 'a-different-build', respond: CHANGED, reloadIsNoop: true });
    live.run();
    await live.advance();
    expect(live.reloads).toBe(1);
    expect(live.fetches).toBe(1);
    // The document is on its way out; nothing here may fetch or reload again.
    for (let attempt = 0; attempt < 3; attempt += 1) await live.advance();
    expect(live.reloads).toBe(1);
    expect(live.fetches).toBe(1);
  });

  it('does not reload at all when the browser will not remember that it did', async () => {
    // Nothing can be remembered across a reload, so a reload cannot be
    // latched: reloading anyway is how a tab reloads every fifteen seconds
    // with no way to stop it.
    const live = world({ shell: 'a-different-build', respond: CHANGED, storageThrows: true });
    live.run();
    for (let attempt = 0; attempt < 4; attempt += 1) await live.advance();
    expect(live.reloads).toBe(0);
    expect(live.warning.textContent).toBe('Not updating.');
    expect(live.armed()).toBe(1);
  });

  it('stops fetching while paused and picks up again when resumed', async () => {
    const live = world();
    live.run();
    const toggle = live.header.children[0];
    if (!toggle) throw new Error('no pause button');

    toggle.handlers.get('click')?.();
    expect(toggle.textContent).toBe('Resume updates');
    await live.advance();
    expect(live.fetches).toBe(0);

    toggle.handlers.get('click')?.();
    expect(toggle.textContent).toBe('Pause updates');
    await settle();
    expect(live.fetches).toBe(1);
  });

  it('drops a reply that lands after the reader pressed pause, and does not fetch again', async () => {
    let deliver: ((page: string) => void) | undefined;
    const live = world({
      respond: () => new Promise((resolve) => {
        deliver = (page) => void resolve({ ok: true, text: () => Promise.resolve(page) });
      })
    });
    live.run();
    const toggle = live.header.children[0];
    if (!toggle) throw new Error('no pause button');

    await live.advance();
    expect(live.fetches).toBe(1);
    toggle.handlers.get('click')?.();
    // The answer to a request the reader has already called off.
    // Rows that would have changed something, so the guard is what stops it.
    deliver?.(laterPage('2026-09-21T12:30:00.000Z', MORE));
    await settle();
    expect(live.main.innerHTML).toBe('');
    expect(live.stamp.getAttribute('datetime')).toBe(SNAPSHOT.generatedAt);
    expect(live.document.title).toBe('stale title');

    await live.advance();
    expect(live.fetches).toBe(1);
  });

  it('falls back to reloading the whole file when it was opened from disk, and that can be paused too', async () => {
    const live = world({ protocol: 'file:' });
    live.run();
    // fetch is blocked there, so there is nothing to poll — but the reader
    // still gets the control every other path gives them.
    expect(live.timers.at(-1)?.delay).toBe(60000);
    const [toggle] = live.header.children;
    expect(toggle?.textContent).toBe('Pause updates');
    toggle?.handlers.get('click')?.();
    await live.advance();
    expect(live.fetches).toBe(0);
    expect(live.reloads).toBe(0);

    toggle?.handlers.get('click')?.();
    await live.advance();
    expect(live.reloads).toBe(1);
  });

  it('still polls on an engine with no AbortSignal.timeout, without the timeout rather than not at all', async () => {
    const live = world({ abortSignal: false, respond: CHANGED });
    live.run();
    await live.advance();
    expect(live.fetches).toBe(1);
    expect(live.requests[0]?.init).toEqual({ cache: 'no-cache', redirect: 'error' });
    expect(live.main.innerHTML).toContain('feature/b');
    expect(live.armed()).toBe(1);
  });

  it('keeps the loop armed, and says so, when the request throws instead of rejecting', async () => {
    const live = world({ throwOnFetch: true });
    live.run();
    for (let attempt = 0; attempt < 4; attempt += 1) {
      await live.advance();
      expect(live.armed()).toBe(1);
    }
    // A page that polls forever and reads nothing must not look healthy.
    expect(live.warning.textContent).toBe('Not updating.');
  });

  it('keeps reading on an engine that rejects the focus-visible selector', async () => {
    const focused = element({}, { focusVisible: true, knowsFocusVisible: false });
    const live = world({ knowsFocusVisible: false, respond: CHANGED });
    live.run();
    live.main.children.push(focused);
    live.document.activeElement = focused;
    await live.advance();
    // Asked once at startup, so a selector the engine refuses cannot throw out
    // of the timer and leave nothing armed. Holding an update is the thing
    // that degrades, not the whole page.
    expect(live.armed()).toBe(1);
    expect(live.fetches).toBe(1);
    expect(live.main.innerHTML).toContain('feature/b');
  });

  it('forgets earlier failures once a read succeeds, so one blip does not read as a stoppage', async () => {
    let up = false;
    const live = world({
      respond: () => up ? CHANGED() : Promise.reject(new Error('router down'))
    });
    live.run();
    for (let attempt = 0; attempt < 4; attempt += 1) await live.advance();
    expect(live.warning.textContent).toBe('Not updating.');

    up = true;
    await live.advance();
    expect(live.warning.textContent).toBe('');

    // One failure after a good read is a blip, not a stoppage.
    up = false;
    await live.advance();
    expect(live.warning.textContent).toBe('');
  });

  it('does not start a second request while one is still outstanding', async () => {
    let deliver: ((page: string) => void) | undefined;
    const live = world({
      respond: () => new Promise((resolve) => {
        deliver = (page) => void resolve({ ok: true, text: () => Promise.resolve(page) });
      })
    });
    live.run();
    await live.advance();
    expect(live.fetches).toBe(1);

    // Coming back to the tab, or resuming, must not race a reply that is still
    // in flight: two of them would write the rows and the fingerprint out of order.
    live.fire('visibilitychange');
    await settle();
    expect(live.fetches).toBe(1);

    deliver?.(laterPage('2026-09-21T12:30:00.000Z', MORE));
    await settle();
    expect(live.main.innerHTML).toContain('feature/b');
  });

  it('does not ask for the page when focus moves and nothing is being held', async () => {
    const live = world();
    live.run();
    live.fire('focusout');
    await settle();
    expect(live.fetches).toBe(0);
    expect(live.armed()).toBe(1);
  });

  it('swaps for a reader whose focus is outside the table', async () => {
    // Parked on the pause button, say: nothing in the table is being read, so
    // nothing is held — the alternative is a page that never updates again.
    const live = world({ respond: CHANGED });
    live.run();
    const [toggle] = live.header.children;
    live.document.activeElement = toggle ?? null;
    await live.advance();
    expect(live.main.innerHTML).toContain('feature/b');
  });

  it('makes a table reachable from the keyboard only while it has somewhere to scroll', async () => {
    const live = world({ respond: CHANGED });
    live.wrap.scrollWidth = 800;
    live.wrap.clientWidth = 400;
    live.run();
    expect(live.wrap.getAttribute('tabindex')).toBe('0');

    // The rows that arrive fit, so the tab stop goes away with them.
    live.wrap.scrollWidth = 400;
    await live.advance();
    expect(live.wrap.getAttribute('tabindex')).toBeNull();
  });

  it('stops promising rows it will not put in while it is paused', async () => {
    const focused = element({}, { focusVisible: true });
    const live = world({ respond: CHANGED });
    live.run();
    live.main.children.push(focused);
    live.document.activeElement = focused;
    await live.advance();
    expect(live.warning.textContent).toBe('New rows waiting.');

    const [toggle] = live.header.children;
    toggle?.handlers.get('click')?.();
    // Paused, nothing will apply them: the marker must not go on saying it will.
    expect(live.warning.textContent).toBe('');

    // Resuming still has them, and puts them in once the reader has left.
    live.document.activeElement = null;
    toggle?.handlers.get('click')?.();
    await live.advance();
    expect(live.main.innerHTML).toContain('feature/b');
  });

  it('keeps saying it cannot read the page while that is still true', async () => {
    const focused = element({}, { focusVisible: true });
    let up = true;
    const live = world({ respond: () => up ? CHANGED() : Promise.reject(new Error('router down')) });
    live.run();
    live.main.children.push(focused);
    live.document.activeElement = focused;
    await live.advance();
    expect(live.warning.textContent).toBe('New rows waiting.');

    up = false;
    for (let attempt = 0; attempt < 4; attempt += 1) await live.advance();
    expect(live.warning.textContent).toBe('Not updating.');

    // Putting the held rows in does not make the page readable again.
    live.document.activeElement = null;
    live.fire('focusout');
    await live.flush();
    expect(live.main.innerHTML).toContain('feature/b');
    expect(live.warning.textContent).toBe('Not updating.');
  });

  it('re-checks whether a table needs a tab stop when the window changes', () => {
    const live = world();
    live.run();
    expect(live.wrap.getAttribute('tabindex')).toBeNull();
    live.wrap.scrollWidth = 800;
    live.wrap.clientWidth = 400;
    live.fire('resize');
    expect(live.wrap.getAttribute('tabindex')).toBe('0');
  });

  it('forgets rows once it has put them in, rather than replaying them later', async () => {
    const focused = element({}, { focusVisible: true });
    let page = () => laterPage('2026-09-21T12:30:00.000Z', MORE);
    const live = world({ respond: () => Promise.resolve({ ok: true, text: () => Promise.resolve(page()) }) });
    live.run();
    live.main.children.push(focused);
    live.document.activeElement = focused;
    await live.advance();

    live.document.activeElement = null;
    live.fire('focusout');
    await live.flush();
    expect(live.main.innerHTML).toContain('feature/b');

    // A third page arrives normally; a later focusout must not put the held
    // rows back over it and wind the clock backwards.
    const third = [...MORE, {
      repositoryId: 'r', worktreeId: 'w3', project: 'acme', domain: 'acme.localhost', slug: 'feature-c',
      host: 'feature-c.acme.localhost', branch: 'feature/c', root: '/work/feature-c',
      status: 'stopped' as const, services: []
    }];
    page = () => laterPage('2026-09-21T13:00:00.000Z', third);
    await live.advance();
    expect(live.main.innerHTML).toContain('feature/c');

    live.fire('focusout');
    await live.flush();
    expect(live.main.innerHTML).toContain('feature/c');
    expect(live.stamp.getAttribute('datetime')).toBe('2026-09-21T13:00:00.000Z');
  });

  it('checks straight away when a hidden tab comes back', async () => {
    const live = world();
    live.run();
    live.document.hidden = true;
    await live.advance();
    expect(live.fetches).toBe(0);
    live.document.hidden = false;
    live.fire('visibilitychange');
    await settle();
    expect(live.fetches).toBe(1);
  });
});
