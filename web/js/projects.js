// The project a view is narrowed to. Chosen in the sessions list or in a report, it is in
// the view's address — so that it can be bookmarked, and Back undoes it — and the links
// from one view to another carry it on, so that one project can be followed through them
// all. The store stays with the list: the reports are of a project's work, and say for
// themselves which stores did it.

import { h, icon, dropdown } from './dom.js';
import { api, onForget } from './api.js';
import * as f from './format.js';

let listP = null;
onForget(() => { listP = null; });

// list is every directory sessions ran in, the most recently used first: asked for once
// in the page's life, by whichever view wants it first.
export function list() {
  listP ||= api('/api/projects').then((r) => r.projects).catch(() => { listP = null; return []; });
  return listP;
}

// within ends a sentence about what a report found when it is narrowed to a project:
// " in work/api".
export const within = (project) => (project ? ` in ${f.shortPath(project)}` : '');

// picker is a report's project: the one chosen, with the way to clear it beside it, and a
// menu of the others. hrefOf(project) is the report's address with that project, and with
// '' for none.
export function picker(chosen, hrefOf) {
  const menu = h('div', { class: 'menu project-menu' });
  const button = h('button', { type: 'button', class: chosen ? 'btn project-btn on' : 'btn project-btn', 'aria-haspopup': 'true', title: chosen || null },
    chosen ? [h('span', { class: 'dim' }, 'Project'), h('span', { class: 'project-name' }, f.shortPath(chosen))] : 'All projects',
    icon('next'));
  const option = (cwd, label, n) => h('a', { href: hrefOf(cwd), title: cwd || null, 'aria-current': cwd === chosen ? 'true' : null },
    h('span', { class: cwd ? 'project-name' : null }, label), n != null && h('span', { class: 'n' }, f.num(n)));
  const draw = (projects) => {
    const shown = projects.filter((p) => p.cwd);
    if (chosen && !shown.some((p) => p.cwd === chosen)) shown.unshift({ cwd: chosen });
    menu.replaceChildren(option('', 'All projects'), ...shown.map((p) => option(p.cwd, f.shortPath(p.cwd), p.sessions)));
  };
  draw([]);
  list().then(draw);
  const drop = dropdown(button, menu);
  // A menu that would run past the edge of the screen opens from the button's other end
  button.addEventListener('click', () => {
    menu.classList.remove('flip');
    if (!menu.hidden && menu.getBoundingClientRect().right > document.documentElement.clientWidth - 8) menu.classList.add('flip');
  });
  return h('div', { class: 'project-pick' },
    drop,
    chosen && h('a', { class: 'icon-btn', href: hrefOf(''), 'aria-label': 'Clear the project', title: 'All projects' }, icon('x')));
}
