// The offline page's Retry: the address asked for again as it is, the turn or step it points
// to with it. A link to "" is the address without its #, and the worker cannot put that
// back: a request never carries it. A script of its own, as the policy allows no other.
document.querySelector('[data-retry]')?.addEventListener('click', (e) => {
  e.preventDefault();
  location.reload();
});
