// The documentation's screenshots, taken from the synthetic history gen.py writes (see
// docs/development.md, "Screenshots"). A Playwright page function: run it with any
// Playwright — the MCP server's browser_run_code, or page.evaluate's caller of your own —
// against `asq serve --port 7882` over that history, from the repository's root.
async (page) => {
  const BASE = 'http://127.0.0.1:7882';
  // Where the images go: this directory's parent, from wherever the repository is
  const DIR = (globalThis.process?.env?.ASQ_SHOTS_DIR || 'docs/screenshots') + '/';
  const HERO = '/s/claude/0e233f41-4192-4051-9807-bebcd81a88ce';
  const ready = async (p) => {
    await p.evaluate(() => document.fonts.ready);
    await p.getByText('up to date').waitFor({ state: 'attached' });
  };
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(BASE + '/');
  // No theme chosen before, and no look before: a browser that has seen this history
  // would mark what changed since, and the pictures are of a first visit
  await page.evaluate(() => localStorage.clear());
  const shot = async (url, file) => {
    await page.goto(BASE + url);
    await ready(page);
    await page.mouse.move(1430, 890);
    await page.waitForTimeout(1500);
    await page.screenshot({ path: DIR + file, scale: 'css' });
  };
  await shot('/', 'sessions.png');
  await shot('/search?q=' + encodeURIComponent('retry backoff'), 'search.png');
  await shot('/usage', 'usage.png');
  await shot('/failures', 'failures.png');
  await shot('/files', 'files.png');
  await shot('/digest', 'digest.png');

  // dark, by the page's own button, then back
  await page.goto(BASE + '/');
  await ready(page);
  await page.getByRole('button', { name: 'Use the dark theme' }).click();
  await page.mouse.move(150, 800);
  await page.waitForTimeout(1200);
  await page.screenshot({ path: DIR + 'sessions-dark.png', scale: 'css' });
  await page.getByRole('button', { name: 'Use the light theme' }).click();
  await page.evaluate(() => { localStorage.removeItem('asq-theme'); delete document.documentElement.dataset.theme; });

  // a session: turn 1 opened, its failed call opened, from the top; then scrolled to the steps
  await page.goto(BASE + HERO);
  await ready(page);
  await page.waitForTimeout(800);
  await page.keyboard.press('j');
  await page.keyboard.press('o');
  const failed = page.locator('.step.tool[data-status="error"] .disc').first();
  await failed.waitFor();
  await page.waitForTimeout(500);
  await failed.click();
  await page.waitForTimeout(800);
  await page.evaluate(() => { document.activeElement && document.activeElement.blur(); window.scrollTo(0, 0); });
  await page.mouse.move(1430, 890);
  await page.waitForTimeout(700);
  await page.screenshot({ path: DIR + 'session.png', scale: 'css' });
  const y = await page.evaluate(() => document.querySelector('.t-day').getBoundingClientRect().top + scrollY);
  await page.evaluate((y) => window.scrollTo(0, y - 66), y);
  await page.mouse.move(1430, 890);
  await page.waitForTimeout(700);
  await page.screenshot({ path: DIR + 'session-steps.png', scale: 'css' });

  // the brief, over the session as it opens
  await page.goto(BASE + HERO);
  await ready(page);
  await page.waitForTimeout(800);
  await page.getByRole('button', { name: 'Brief' }).click();
  await page.waitForTimeout(1500);
  await page.mouse.move(1000, 880);
  await page.waitForTimeout(500);
  await page.screenshot({ path: DIR + 'brief.png', scale: 'css' });

  // a phone: its own context, so that it is a phone and not a narrow desk
  const ctx = await page.context().browser().newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, colorScheme: 'light' });
  const p = await ctx.newPage();
  try {
    await p.goto(BASE + HERO);
    await ready(p);
    await p.waitForTimeout(800);
    await p.screenshot({ path: DIR + 'phone-session.png', scale: 'css' });
  } finally {
    await ctx.close();
  }
  return 'done';
}
