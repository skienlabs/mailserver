// Applied before first paint so the page never flashes the wrong theme.
(function () {
  var pref = 'system';
  try { pref = localStorage.getItem('sm.theme') || 'system'; } catch (e) {}
  var dark = pref === 'dark' || (pref === 'system' && window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  // The background colours follow the local time of day.
  function daypart() {
    var h = new Date().getHours();
    var p = h >= 5 && h < 9 ? 'dawn' : h >= 9 && h < 17 ? 'day' : h >= 17 && h < 21 ? 'dusk' : 'night';
    if (document.documentElement.getAttribute('data-daypart') !== p) document.documentElement.setAttribute('data-daypart', p);
  }
  daypart();
  setInterval(daypart, 5 * 60 * 1000);
})();
