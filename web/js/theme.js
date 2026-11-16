// Runs before the first paint, so that a chosen theme never flashes the other one.
try {
  const theme = localStorage.getItem('asq-theme');
  if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
} catch { /* storage refused: the system's choice stands */ }
