// Where a session's latest turn stands, as its transcript says it (docs/model.md, "Where a
// turn stands"): the agent at work, waiting on a call, or done and waiting for the person.
// Said only for the CLIs that record it, and only while the transcript was written recently
// enough to believe it; a session it is not said of is marked live while it is written to.

import { h } from './dom.js';
import * as f from './format.js';

// What the list is narrowed to for the sessions that wait on the person.
export const NEEDS_YOU = 'waiting,idle';

const since = (l) => (l.since ? ` since ${f.stamp(f.when(l.since))}` : '');

// The mark of a session at work, or written to just now: a green dot, and what it says. The
// dot is the mark's own content, so that in a line of facts the separator comes before it
// and not between it and its word.
const live = (title, text) => h('span', { class: 'live', title }, h('span', { class: 'live-dot', 'aria-hidden': 'true' }), text);

// standing is the mark a session's facts carry, or null. href, for a session that waits on
// a call, makes the mark a link to that call.
export function standing(s, href) {
  const l = s.live;
  if (!l) return s.active ? live('Written to in the last two minutes', 'live') : null;
  switch (l.state) {
    case 'working':
      return live(`At work${since(l)}: the model answering, or a result on its way back to it`, 'working');
    case 'waiting': {
      const what = l.command || (l.file ? f.shortPath(l.file) : '');
      const label = `waiting on ${l.tool || 'a call'}${what ? ': ' + f.firstLine(what, 60) : ''}`;
      const title = `${label}${since(l)} — a command still running, or a prompt asking permission to run it`;
      return h(href ? 'a' : 'span', { class: 'standing on-call', href, title }, label);
    }
    case 'idle':
      return h('span', { class: 'standing yours', title: `The agent's turn is over${since(l)}: the next move is yours` }, 'your turn');
  }
  return null;
}
