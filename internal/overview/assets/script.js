
    function planIndexUpdate(current               , html        )              {
  // Every sentinel is split, because this function's own source is embedded in
  // the page it parses: spelled out whole, each would also match the copy of
  // itself further down the document.
  const mainOpen = '<' + 'main';
  const mainClose = '</' + 'main>';
  const titleOpen = '<' + 'title>';
  const titleClose = '</' + 'title>';
  const stampTag = '<' + 'time datetime="';
  const shellTag = 'data-' + 'shell="';
  const bodyTag = 'data-' + 'body="';
  // An attribute value, refused when the tag ends before the closing quote: a
  // response cut mid-attribute must not read as the rest of the document.
  function value(tag        , from        )                     {
    const at = html.indexOf(tag, from);
    if (at < 0) return undefined;
    const quote = html.indexOf('"', at + tag.length);
    const tagEnd = html.indexOf('>', at + tag.length);
    if (quote < 0 || (tagEnd >= 0 && tagEnd < quote)) return undefined;
    return html.slice(at + tag.length, quote);
  }
  const mainAt = html.indexOf(mainOpen);
  const titleAt = html.indexOf(titleOpen);
  if (mainAt < 0 || titleAt < 0) return { kind: 'foreign' };
  const mainTagEnd = html.indexOf('>', mainAt);
  const mainEnd = html.indexOf(mainClose, mainAt);
  const titleEnd = html.indexOf(titleClose, titleAt);
  const stamp = value(stampTag, 0);
  const shell = value(shellTag, 0);
  const body = value(bodyTag, mainAt);
  if (mainTagEnd < 0 || mainEnd < mainTagEnd || titleEnd < 0) return { kind: 'foreign' };
  if (stamp === undefined || stamp === '' || shell === undefined || body === undefined) return { kind: 'foreign' };
  if (stamp === current) return { kind: 'unchanged' };
  return {
    kind: 'update',
    stamp,
    shell,
    body,
    title: html.slice(titleAt + titleOpen.length, titleEnd),
    main: html.slice(mainTagEnd + 1, mainEnd)
  };
}
    (function () {
      var stamp = document.querySelector('header time');
      var header = document.querySelector('header');
      var announcer = document.getElementById('index-announcer');
      var warning = document.getElementById('index-stale');
      if (!stamp || !header) return;
      function localise(el) {
        var when = new Date(el.getAttribute('datetime') || '');
        if (!isNaN(when.getTime())) el.textContent = when.toLocaleString();
      }
      localise(stamp);
      // A table only needs to be reachable from the keyboard while it has
      // something off-screen to scroll to.
      function reachable() {
        var wraps = document.querySelectorAll('.table-wrap');
        for (var index = 0; index < wraps.length; index += 1) {
          if (wraps[index].scrollWidth > wraps[index].clientWidth) wraps[index].setAttribute('tabindex', '0');
          else wraps[index].removeAttribute('tabindex');
        }
      }
      reachable();
      window.addEventListener('resize', reachable);
      var paused = false;
      var toggle = document.createElement('button');
      toggle.type = 'button';
      toggle.className = 'pause';
      // The name carries the state, so there is no aria-pressed to contradict
      // it: a toggle may change one or the other, never both.
      function label() { toggle.textContent = paused ? 'Resume updates' : 'Pause updates'; }
      // Opened from disk rather than through the router: fetch is blocked, so
      // the only thing left is to reload the whole file — and the reader still
      // gets to stop it.
      if (location.protocol === 'file:') {
        var reloadTimer;
        // A hidden tab is not being read; reloading it would only throw away
        // the reader's place before they come back to it.
        function again() {
          reloadTimer = setTimeout(function () {
            if (document.hidden) again(); else location.reload();
          }, 60000);
        }
        toggle.addEventListener('click', function () {
          paused = !paused;
          label();
          if (paused) clearTimeout(reloadTimer); else again();
        });
        label();
        header.appendChild(toggle);
        again();
        return;
      }
      // Asked once: an engine that does not know the selector throws for it,
      // and a throw inside the loop is how a page stops updating for good.
      var knowsFocusVisible = (function () {
        try {
          document.documentElement.matches(':focus-visible');
          return true;
        } catch (error) {
          return false;
        }
      })();
      var inFlight = false;
      var failures = 0;
      var waiting = null;
      var reloaded = false;
      var timer;
      function schedule() { clearTimeout(timer); timer = setTimeout(tick, 15000); }
      // Written only when it changes: it is a live region, and assigning the
      // same sentence every fifteen seconds announces it again.
      function marker(text, kind) {
        if (!warning || warning.textContent === text) return;
        warning.textContent = text;
        warning.className = kind;
      }
      // Four quiet failures is a page that has stopped working, not a blip.
      function failed() {
        failures = failures + 1;
        if (failures >= 4) marker('Not updating.', 'stale');
      }
      // Keyed by the shell it reloaded for, so a tab that sees two builds
      // alternate does not reload every fifteen seconds; the in-memory flag
      // covers a browser that refuses to store anything.
      function reloadOnce(shell) {
        var key = 'treerig-shell:' + shell;
        try {
          if (sessionStorage.getItem(key)) { marker('Not updating.', 'stale'); return; }
          sessionStorage.setItem(key, '1');
        } catch (error) {
          // Nothing can be remembered across a reload, so a reload cannot be
          // latched: reloading anyway is how a tab ends up reloading every
          // fifteen seconds with no way to stop it. Say so instead.
          marker('Not updating.', 'stale');
          return;
        }
        reloaded = true;
        location.reload();
      }
      function apply(main, plan) {
        waiting = null;
        if (plan.body !== main.getAttribute('data-body')) {
          main.innerHTML = plan.main;
          main.setAttribute('data-body', plan.body);
          reachable();
          if (announcer) announcer.textContent = plan.title + ' updated.';
        }
        if (failures === 0) marker('', '');
        stamp.setAttribute('datetime', plan.stamp);
        localise(stamp);
        document.title = plan.title;
      }
      // Only a keyboard reader loses their place to a swap. A click leaves
      // focus on the link just followed, and must not hold anything up.
      function held(main) {
        var active = document.activeElement;
        return !!(knowsFocusVisible && active && main.contains(active) &&
          active.matches && active.matches(':focus-visible'));
      }
      function tick() {
        clearTimeout(timer);
        // Once a reload is asked for this page is finished; a browser takes a
        // moment to act on it, and nothing should poll in the meantime.
        if (paused || inFlight || reloaded) return;
        try {
          var main = document.querySelector('main');
          // A hidden tab does not need the traffic.
          if (document.hidden || !main) { schedule(); return; }
          var request = { cache: 'no-cache', redirect: 'error' };
          if (typeof AbortSignal !== 'undefined' && AbortSignal.timeout) {
            request.signal = AbortSignal.timeout(10000);
          }
          inFlight = true;
          fetch(location.pathname, request)
            .then(function (response) {
              return response.ok ? response.text() : Promise.reject(new Error(String(response.status)));
            })
            .then(function (html) {
              var plan = planIndexUpdate(stamp.getAttribute('datetime'), html);
              // Something answered, but it is not this page: that is a failure
              // to read the index, however healthy the response looked.
              if (plan.kind === 'foreign') { failed(); return; }
              failures = 0;
              if (plan.kind === 'unchanged') { marker('', ''); return; }
              if (paused) return;
              // A newer treerig wrote that page: its markup belongs to a
              // stylesheet and a script this tab does not have.
              if (plan.shell !== document.documentElement.getAttribute('data-shell')) {
                reloadOnce(plan.shell);
                return;
              }
              // Every regeneration moves the stamp, most change nothing on the
              // page: advance the clock quietly, and only swap — losing a
              // selection, spending an announcement — for a real change.
              // Reading inside the table: keep the rows rather than pulling
              // them out from under them, and put them in as soon as the
              // reader leaves. The stamp stays put until they are shown.
              if (plan.body !== main.getAttribute('data-body') && held(main)) {
                waiting = plan;
                marker('New rows waiting.', 'waiting');
                return;
              }
              apply(main, plan);
            })
            .catch(function () {
              // The router is down, or a regeneration is in flight: the next
              // tick will do. Say so only once it has clearly stopped working.
              failed();
            })
            .then(function () { inFlight = false; schedule(); });
        } catch (error) {
          // Nothing may leave the loop unarmed, and nothing may leave it
          // looking healthy while it fetches nothing.
          inFlight = false;
          failed();
          schedule();
        }
      }
      toggle.addEventListener('click', function () {
        paused = !paused;
        label();
        // The held rows are kept, but the marker promising them is not: while
        // paused nothing will put them in.
        if (paused) { clearTimeout(timer); if (waiting) marker('', ''); } else tick();
      });
      label();
      header.appendChild(toggle);
      document.addEventListener('visibilitychange', function () { if (!document.hidden && !paused) tick(); });
      // Rows held for a reader go in the moment they leave the table, rather
      // than waiting out the rest of the interval. Nothing is fetched here:
      // focus moves within a table would otherwise pull the page per keystroke.
      document.addEventListener('focusout', function () {
        if (paused || !waiting) return;
        // focusout runs before focus lands, so let it.
        setTimeout(function () {
          var main = document.querySelector('main');
          if (!paused && waiting && main && !held(main)) apply(main, waiting);
        }, 0);
      });
      schedule();
    })();
  