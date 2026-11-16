// The one thing a token-protected server shows before it is given its token. The token
// is traded once for a cookie that script never sees.

import { h } from '../dom.js';
import { api } from '../api.js';

export function login(main) {
  document.title = 'Sign in · asq';
  const input = h('input', { type: 'password', name: 'token', id: 'token', required: true, autocomplete: 'current-password', spellcheck: false });
  const said = h('p', { class: 'login-error', role: 'alert' });
  const button = h('button', { type: 'submit', class: 'btn primary' }, 'Sign in');
  const form = h('form', { class: 'login', 'aria-labelledby': 'login-title' },
    h('h1', { id: 'login-title' }, 'asq'),
    h('p', { class: 'dim' }, 'This server reads every agent session on its machine, so it asks for its token first.'),
    h('label', { for: 'token' }, 'Token'),
    input, said, button,
    h('p', { class: 'dim small' }, 'The token is the one the server was started with (--token or ASQ_TOKEN).'));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    button.disabled = true;
    said.textContent = '';
    try {
      await api('/api/login', { method: 'POST', body: { token: input.value } });
      location.reload();
    } catch (err) {
      said.textContent = err.status === 401 ? 'That is not the token.' : err.message;
      button.disabled = false;
      input.select();
    }
  });
  main.replaceChildren(form);
  input.focus();
}
