// Applies the theme you chose before the page first paints.
//
// The stylesheet follows the system until the root element carries data-theme, and app.js
// (the toggle) loads at the end of the body — after the first paint. Someone who pinned the
// other theme would see the system's for a moment on every load. This runs in <head>, is
// a few lines, and only reads what the toggle stored; it is a separate file because the
// page's Content-Security-Policy allows no inline script.
'use strict';
(function () {
  try {
    var theme = localStorage.getItem('agent-session-query-theme');
    if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
  } catch (e) {
    // storage unavailable: follow the system
  }
})();
