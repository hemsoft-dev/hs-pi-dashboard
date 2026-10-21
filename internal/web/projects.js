/* gh-x owns all status verdicts. This view only groups machines and renders fields. */
(function(root) {
  const esc = value => String(value ?? "").replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
  const link = (url, text) => {
    try { const u = new URL(url); if (u.protocol === 'https:' && u.hostname === 'github.com' && !u.username && !u.password) return `<a href="${esc(u.href)}" target="_blank" rel="noopener noreferrer">${esc(text)}</a>`; } catch {}
    return esc(text);
  };
  const date = value => { const d = new Date(value); return value && Number.isFinite(+d) ? d.toLocaleString('en-US', {timeZone:'America/New_York'}) + ' ET' : 'pending'; };
  const healthKeys = ['defaultBranch','currentBranch','branches','worktrees','stashes'];
  function localClean(local) {
    return local.online && !local.error && local.document && healthKeys.every(k => local.document.health?.[k]?.status === 'available' && local.document.health[k].clean === true);
  }
  function table(rows, columns, label) {
    if (!rows?.length) return '<p class="pnote">None</p>';
    return `<div class="ptable-wrap" role="region" aria-label="${esc(label)}" tabindex="0"><table><thead><tr>${columns.map(([,title]) => `<th scope="col">${esc(title)}</th>`).join('')}</tr></thead><tbody>${rows.map(row => `<tr>${columns.map(([key]) => `<td>${key === 'number' || key === 'id' || key === 'title' ? link(row.url, key === 'number' ? '#'+row[key] : row[key]) : esc(typeof row[key] === 'object' && row[key] !== null ? JSON.stringify(row[key]) : row[key] ?? 'unknown')}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`;
  }
  const prs = [['number','PR'],['issues','Issues'],['title','Title'],['author','Author'],['state','State'],['review','Review'],['aiReview','AI'],['aiClean','AI clean'],['approvals','Approvals'],['approvers','Approvers'],['approvalsIncomplete','Approvals incomplete'],['checks','Checks'],['comments','Comments'],['branch','Branch'],['updated','Updated']];
  function githubSection(doc, key, title, rows, columns) {
    const meta = doc.sections?.find(s => s.name === key);
    const state = !meta ? 'unavailable' : meta.status;
    return `<section class="pgroup"><h4>${esc(title)} <span class="pnote">${state === 'available' ? `${rows?.length || 0} · ${meta.cached ? 'cached' : 'fetched'} ${date(meta.fetchedAt)}` : esc(state)}</span></h4>${state === 'unavailable' ? `<p class="perror">${esc(meta?.reason || 'section metadata missing')}</p>` : state === 'disabled' ? '<p class="pnote">Disabled by gh-x configuration</p>' : table(rows,columns,title)}</section>`;
  }
  function health(local) {
    if (!local.document) return '';
    const h = local.document.health;
    const labels = {defaultBranch:'Default branch',currentBranch:'Current branch',branches:'Branches',worktrees:'Worktrees',stashes:'Stashes'};
    return `<dl class="phealth">${healthKeys.map(key => {
      const r = h?.[key];
      if (!r || r.status !== 'available') return `<div><dt>${labels[key]}</dt><dd class="perror">Unavailable · ${esc(r?.reason || 'no reading')}</dd></div>`;
      let detail = '';
      if (key.endsWith('Branch')) detail = `${r.name || 'detached HEAD'} · ${r.upstream || 'no upstream'} · ${r.ahead} ahead / ${r.behind} behind · ${r.changes?.clean ? 'working tree clean' : 'working changes'} · ${r.checkedOut ? 'checked out' : 'not checked out'}`;
      if (key === 'branches') detail = `${r.local} local · ${r.remote} remote · ${r.dangling} dangling`;
      if (key === 'worktrees') detail = `${r.total} total · ${r.cleanupCandidates} cleanup candidates`;
      if (key === 'stashes') detail = `${r.count} stashes`;
      return `<div><dt>${labels[key]}</dt><dd><span class="${r.clean ? '' : 'perror'}">${r.clean ? 'Clean' : 'Attention'}</span> · ${esc(detail)}</dd></div>`;
    }).join('')}</dl>`;
  }
  function repository(repo) {
    const doc = repo.github;
    const attention = repo.locals.some(l => !localClean(l)) || !doc || doc.sections?.some(s => s.status === 'unavailable');
    const count = (key, rows) => doc?.sections?.find(s => s.name === key)?.status === 'available' ? rows?.length || 0 : 'unavailable';
    return `<details class="prepo" data-project="${esc(repo.name)}"><summary><span class="pname">${esc(repo.name)}</span><span class="pstate ${attention ? 'perror' : ''}">${attention ? 'Attention' : 'Clean'}</span><span class="pcounts">${count('pullRequests.open',doc?.pullRequests?.open)} PRs · ${count('issues',doc?.issues)} issues · ${repo.locals.length} checkouts</span></summary><div class="pdetail">
      <h3>${link(doc?.repository?.nameWithOwner ? 'https://github.com/'+doc.repository.nameWithOwner : '', repo.name)}</h3>
      ${repo.locals.map(local => `<section class="plocal"><h4>${esc(local.machine)} <span class="pnote">${local.online ? 'online' : 'offline · cached'}</span></h4><p class="pnote ppath">${esc(local.path)} · collected ${date(local.document?.generatedAt)}</p>${local.error ? `<p class="perror">${esc(local.error)}${local.document ? ' · showing cached data' : ''}</p>` : ''}${health(local)}</section>`).join('')}
      ${doc ? `<p class="pnote">GitHub data shown once per repository · snapshot ${date(doc.generatedAt)}</p>`
      + githubSection(doc,'pullRequests.open','Open pull requests',doc.pullRequests?.open,prs)
      + githubSection(doc,'issues','Open issues',doc.issues,[['number','Issue'],['pullRequests','PRs'],['parent','Parent'],['subIssues','Sub'],['title','Title'],['author','Author'],['state','State'],['labels','Labels'],['assignees','Assignees'],['updated','Updated']])
      + githubSection(doc,'pullRequests.merged','Recently merged pull requests',doc.pullRequests?.merged,prs)
      + githubSection(doc,'workflowRuns','Recent workflow runs',doc.workflowRuns,[['id','Run'],['status','Status'],['rawStatus','Raw status'],['title','Title'],['workflow','Workflow'],['branch','Branch'],['event','Event'],['elapsed','Elapsed'],['age','Age']]) : ''}
    </div></details>`;
  }
  function init() {
    const button=document.getElementById('projects-toggle'), body=document.getElementById('projects-body');
    try { body.hidden=localStorage.getItem('hs-pi-dashboard:projects-open') === 'false'; } catch {}
    button.setAttribute('aria-expanded',String(!body.hidden));
    button.addEventListener('click',()=>{ body.hidden=!body.hidden; button.setAttribute('aria-expanded',String(!body.hidden)); try { localStorage.setItem('hs-pi-dashboard:projects-open',String(!body.hidden)); } catch {} });
  }
  function render(view) {
    const body=document.getElementById('projects-body');
    const open=new Set([...body.querySelectorAll('.prepo[open]')].map(e=>e.dataset.project));
    const focused=body.contains(document.activeElement) ? document.activeElement.closest('.prepo')?.dataset.project : null;
    const focusedRepo=[...body.querySelectorAll('.prepo')].find(e=>e.dataset.project===focused);
    const focusIndex=focusedRepo ? [...focusedRepo.querySelectorAll('summary,a,[tabindex]')].indexOf(document.activeElement) : -1;
    body.innerHTML=(view?.errors || []).map(e=>`<p class="perror pnotice">${esc(e)}</p>`).join('')+(view?.repositories || []).map(repository).join('') || '<p class="pnote">No projects configured. Set -project-path on an agent.</p>';
    for (const el of body.querySelectorAll('.prepo')) { el.open=open.has(el.dataset.project); if (el.dataset.project===focused) (el.querySelectorAll('summary,a,[tabindex]')[focusIndex] || el.querySelector('summary')).focus({preventScroll:true}); }
    document.getElementById('projects-summary').textContent=`${view?.repositories?.length || 0} repositories`;
  }
  root.ProjectsDashboard={init,render,repository,localClean,link};
  if (typeof module !== 'undefined') module.exports=root.ProjectsDashboard;
})(typeof window === 'undefined' ? globalThis : window);
