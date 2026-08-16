// Docker container picker for the job editor.
//
// When the client is a Docker host, the job editor offers its containers as
// tick-boxes and derives the paths — and, for databases, the dump hooks —
// so a Docker host can be configured without knowing volume paths.
//
// This lives in a static file rather than inline in job_edit.html on
// purpose: html/template's contextual JavaScript escaper rewrites
// backslashes inside <script> blocks, which corrupts the shell quoting in
// the generated hooks (`'\''` came out as `\'`). Static files are served
// verbatim.

const STAGING = '/var/lib/backup-agent/staging';

const DockerPicker = (() => {
  let inv = null;
  let form = null;
  let agentID = '';
  // False once the hooks differ from what the picker would generate: the
  // admin has edited them, so ticking must not overwrite their work.
  let ownsHooks = true;
  // Paths this panel contributed, so they can be withdrawn on untick
  // without disturbing anything typed by hand.
  let added = new Set();

  // dumpFor returns the dump file plus pre/post hooks for a database
  // container, or null when the engine can't be dumped generically.
  function dumpFor(c) {
    const f = `${STAGING}/${c.name}.sql`;
    const mk = `mkdir -p ${STAGING} && `;
    switch (c.engine) {
      case 'mysql':
        return {file: f, todo: false, post: `rm -f ${f}`,
          pre: `${mk}docker exec ${c.name} sh -c 'exec mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --single-transaction --routines --triggers --events --all-databases' > ${f}`};
      case 'postgres':
        return {file: f, todo: false, post: `rm -f ${f}`,
          pre: `${mk}docker exec ${c.name} sh -c 'exec pg_dumpall -U "$POSTGRES_USER"' > ${f}`};
      case 'mongo': {
        const a = `${STAGING}/${c.name}.archive`;
        return {file: a, todo: false, post: `rm -f ${a}`,
          pre: `${mk}docker exec ${c.name} sh -c 'exec mongodump --archive' > ${a}`};
      }
      case 'mssql': {
        // SQL Server can't stream a backup to stdout: it writes a .bak
        // inside its own volume, which is then backed up via the host
        // mount. The database name can't be guessed, so this is a template
        // the admin completes. The T-SQL path is single-quoted inside a
        // single-quoted bash -c argument, hence the '\'' sequences.
        const m = (c.mounts || []).find(x => x.destination === '/var/opt/mssql') || (c.mounts || [])[0];
        if (!m) return null;
        return {file: `${m.backup_path}/backup/DATABASE.bak`, todo: true, post: '',
          pre: `docker exec ${c.name} mkdir -p /var/opt/mssql/backup && docker exec ${c.name} bash -c '/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -C -Q "BACKUP DATABASE [DATABASE] TO DISK='\\''/var/opt/mssql/backup/DATABASE.bak'\\'' WITH INIT, FORMAT"'`};
      }
      case 'redis': {
        // Redis persists to its own volume: force a save, then copy it.
        const m = (c.mounts || [])[0];
        if (!m) return null;
        return {file: m.backup_path, todo: false, post: '',
          pre: `docker exec ${c.name} redis-cli SAVE`};
      }
    }
    return null;
  }

  function selected() {
    return [...document.querySelectorAll('#docker-list input[data-name]:checked')]
      .map(i => inv.containers.find(c => c.name === i.dataset.name))
      .filter(Boolean);
  }

  // build derives the paths and hooks a selection implies. Pure: it reads
  // the tick state but writes nothing, so it can also be used to work out
  // whether a saved job's hooks are still the ones the picker would produce.
  function build(sel) {
    // Only one database per job: a job carries a single pre-hook.
    const dbs = sel.filter(c => c.kind === 'database' && dumpFor(c));
    const paths = [], pre = [], post = [];
    let todo = false;
    for (const c of sel) {
      if (c.kind === 'database') {
        const d = dumpFor(c);
        if (!d || (dbs[0] && c.name !== dbs[0].name)) continue;
        paths.push(d.file);
        if (d.pre) pre.push(d.pre);
        if (d.post) post.push(d.post);
        todo = todo || d.todo;
        continue;
      }
      for (const m of c.mounts || []) paths.push(m.backup_path);
      if (document.querySelector(`#docker-list input[data-stop="${cssEscape(c.name)}"]`)?.checked) {
        pre.push(`docker stop ${c.name}`);
        post.unshift(`docker start ${c.name}`); // restart first, clean up after
      }
    }
    return {paths, pre: pre.join(' && '), post: post.join(' && '), todo, dbs};
  }

  // apply recomputes paths — and, when the picker still owns them, hooks —
  // from the current tick state.
  function apply() {
    const sel = selected();
    const out = build(sel);

    document.querySelectorAll('#docker-list input[data-db="1"]').forEach(i => {
      i.disabled = out.dbs.length > 0 && i.dataset.name !== out.dbs[0].name;
    });
    el('docker-db-warn').hidden = out.dbs.length === 0;

    // Withdraw only the lines this panel contributed, so hand-typed paths
    // survive ticking and unticking.
    const kept = form.paths.value.split('\n').map(s => s.trim())
      .filter(s => s && !added.has(s));
    form.paths.value = [...new Set([...kept, ...out.paths])].join('\n');
    added = new Set(out.paths);

    if (ownsHooks) {
      form.pre_hook.value = out.pre;
      form.post_hook.value = out.post;
    }
    el('docker-hooks-warn').hidden = ownsHooks;
    el('docker-hint').innerHTML = out.todo
      ? '⚠ Replace <code>DATABASE</code> in the pre-hook and in the path with the name of the database to back up.'
      : 'Tick what to include — paths (and database dump hooks) are filled in for you.';
  }

  // regenerateHooks discards hand edits and takes ownership back.
  function regenerateHooks() {
    ownsHooks = true;
    apply();
  }

  function cssEscape(s) { return String(s).replace(/["\\]/g, '\\$&'); }
  function reEscape(s) { return String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }

  // adopt ticks the containers a saved job already covers, so reopening a
  // job shows its real state and containers can be added or removed without
  // starting over.
  //
  // Databases are matched on the hook rather than the path: the generated
  // SQL Server path contains a placeholder the admin replaces, so the path
  // won't match, but `docker exec <name>` still will. File and app
  // containers are matched on their paths all being present — a partial
  // match means the job was hand-built and shouldn't be claimed.
  function adopt() {
    const paths = new Set(form.paths.value.split('\n').map(s => s.trim()).filter(Boolean));
    const pre = form.pre_hook.value;

    for (const c of inv.containers) {
      const box = document.querySelector(`#docker-list input[data-name="${cssEscape(c.name)}"]`);
      if (!box || box.disabled) continue;
      const execRe = new RegExp(`docker exec ${reEscape(c.name)}(\\s|$)`);
      if (c.kind === 'database') {
        box.checked = execRe.test(pre) || new RegExp(`docker exec ${reEscape(c.name)} redis-cli`).test(pre);
        continue;
      }
      const mine = (c.mounts || []).map(m => m.backup_path);
      box.checked = mine.length > 0 && mine.every(p => paths.has(p));
      if (box.checked) {
        const stop = document.querySelector(`#docker-list input[data-stop="${cssEscape(c.name)}"]`);
        if (stop) stop.checked = new RegExp(`docker stop ${reEscape(c.name)}(\\s|$)`).test(pre);
      }
    }

    // The picker only keeps writing the hooks if the saved ones are exactly
    // what it would have produced. Anything else is a hand edit — an
    // mssql database name filled in, an extra command appended — and must
    // survive later ticking.
    const out = build(selected());
    ownsHooks = form.pre_hook.value.trim() === out.pre.trim() &&
                form.post_hook.value.trim() === out.post.trim();
    added = new Set(out.paths);

    document.querySelectorAll('#docker-list input[data-db="1"]').forEach(i => {
      i.disabled = out.dbs.length > 0 && i.dataset.name !== out.dbs[0].name;
    });
    el('docker-db-warn').hidden = out.dbs.length === 0;
    el('docker-hooks-warn').hidden = ownsHooks;
  }

  const KIND_LABEL = {
    database: 'database — dumped',
    embedded: 'app database — copied',
    files: 'files',
    stateless: 'no data to back up',
  };

  function render() {
    const list = el('docker-list');
    list.innerHTML = '';
    const stacks = {};
    for (const c of inv.containers) (stacks[c.stack || ''] ||= []).push(c);

    for (const stack of Object.keys(stacks).sort()) {
      const box = document.createElement('div');
      box.style.margin = '.4rem 0';
      if (stack) box.innerHTML = `<div class="muted" style="margin:.3rem 0"><b>${esc(stack)}</b></div>`;
      for (const c of stacks[stack]) {
        const usable = c.kind !== 'stateless';
        const paths = (c.mounts || []).map(m => m.backup_path);
        const row = document.createElement('div');
        row.style.margin = '.15rem 0 .15rem .6rem';
        row.innerHTML =
          `<label class="check"${usable ? '' : ' style="opacity:.55"'}>` +
          `<input type="checkbox" data-name="${esc(c.name)}"` +
          `${c.kind === 'database' ? ' data-db="1"' : ''}${usable ? '' : ' disabled'}> ` +
          `<b>${esc(c.name)}</b> <span class="muted">${esc(c.image)} · ${esc(KIND_LABEL[c.kind] || c.kind)}` +
          `${c.state && c.state !== 'running' ? ' · ' + esc(c.state) : ''}</span></label>` +
          (paths.length ? `<div class="muted" style="margin-left:1.6rem;font-size:.85em">${paths.map(esc).join('<br>')}</div>` : '') +
          (c.kind === 'embedded'
            ? `<label class="check" style="margin-left:1.6rem;font-size:.85em"><input type="checkbox" data-stop="${esc(c.name)}"> stop the container during backup (guaranteed consistency, brief downtime)</label>`
            : '') +
          (c.note ? `<div class="err" style="margin-left:1.6rem;font-size:.85em">⚠ ${esc(c.note)}</div>` : '');
        box.appendChild(row);
      }
      list.appendChild(box);
    }
    list.querySelectorAll('input[type=checkbox]').forEach(i => i.addEventListener('change', apply));
    el('docker-hint').hidden = false;
    el('docker-panel').hidden = false;
    adopt();
  }

  // status shows a message in place of the container list. Failures have to
  // be visible: hiding the whole panel on error leaves the admin with no
  // feedback, no Refresh button and nothing to diagnose from.
  function status(msg, isError) {
    el('docker-panel').hidden = false;
    el('docker-db-warn').hidden = true;
    el('docker-hint').hidden = true;
    el('docker-list').innerHTML =
      `<p class="${isError ? 'err' : 'muted'}" style="margin:.3rem 0">${esc(msg)}</p>`;
  }

  // loadHint turns a failed discovery into the likely cause. Both common
  // causes are version skew after an upgrade, which is otherwise invisible.
  function loadHint(err) {
    const m = String(err && err.message || err);
    if (/did not respond in time/i.test(m)) {
      return 'The client did not answer. This usually means the agent is older than the ' +
        'server and does not understand container discovery yet — update the agent image ' +
        'and redeploy it. (Its log will show: unknown message type "discover_docker".)';
    }
    if (/offline|could not reach/i.test(m)) {
      return 'The client is offline, so its containers cannot be listed.';
    }
    return 'Could not list containers: ' + m;
  }

  // load fetches the client's container inventory. A client that genuinely
  // isn't a Docker host gets no panel at all; anything that *failed* gets a
  // visible explanation.
  async function load(id, f, explicit) {
    agentID = id || agentID;
    form = f || form;
    if (!agentID) return;
    status('Looking for Docker containers on this client…', false);
    try {
      const got = await api('GET', `/api/admin/agents/${agentID}/docker`);
      if (got.available && (got.containers || []).length) {
        inv = got;
        render();
        return;
      }
      if (got.error) {
        status('Docker could not be read on this client: ' + got.error, true);
        return;
      }
      if (got.available) {
        status('Docker is running on this client, but it has no containers.', false);
        return;
      }
      // No Docker socket: an ordinary non-Docker client, so no panel.
      el('docker-panel').hidden = true;
    } catch (err) {
      status(loadHint(err), true);
      if (explicit) toast(err.message);
    }
  }

  return {load, regenerateHooks, _dumpFor: dumpFor};
})();
