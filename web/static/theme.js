// Applies the chosen theme before first paint, so the page never flashes the default.
// The choice lives in a cookie rather than localStorage: each launch serves on a new
// port, and cookies, unlike storage, are shared by every port of a host.
{
  const theme = document.cookie.match(/(?:^|; )theme=([^;]*)/)?.[1];
  if (theme) document.documentElement.dataset.theme = theme;
}
