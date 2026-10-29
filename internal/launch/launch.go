// Package launch opens the UI in a standalone browser app window.
package launch

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// A dedicated profile makes the browser start its own process. Otherwise the launch is
// handed to an already running browser, which ignores window flags like --start-maximized
// and restores the app window's last saved bounds instead.
var browserFlags = []string{"--start-maximized", "--no-first-run", "--no-default-browser-check"}

const profileDir = "Skillboard/Browser"

// App mode (a window without tabs or an address bar) needs a Chromium-based browser.
// Under WSL the Windows browsers are preferred: they reach WSL's localhost directly.
// Each list is in order of preference; the first one installed wins.
var (
	windowsBrowsers = []string{
		"/mnt/c/Program Files/BraveSoftware/Brave-Browser/Application/brave.exe",
		"/mnt/c/Program Files/Google/Chrome/Application/chrome.exe",
		"/mnt/c/Program Files (x86)/Google/Chrome/Application/chrome.exe",
		"/mnt/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
		"/mnt/c/Program Files/Microsoft/Edge/Application/msedge.exe",
	}
	linuxBrowsers = []string{"brave-browser", "google-chrome", "chromium", "chromium-browser", "microsoft-edge"}
	// Plain-tab fallbacks when no Chromium browser is around.
	urlOpeners = []string{"wslview", "xdg-open"}
)

func OpenApp(url string) error {
	appArgs := append([]string{"--app=" + url}, browserFlags...)

	if os.Getenv("WSL_DISTRO_NAME") != "" {
		for _, bin := range windowsBrowsers {
			if _, err := os.Stat(bin); err == nil {
				appData, err := windowsLocalAppData()
				if err != nil {
					// Still opens, just without --start-maximized taking effect.
					slog.Warn("no dedicated browser profile", "err", err)
					return start(bin, windowsDir, appArgs...)
				}
				profile := appData + `\` + strings.ReplaceAll(profileDir, "/", `\`)
				return start(bin, windowsDir, append(appArgs, "--user-data-dir="+profile)...)
			}
		}
	}
	for _, name := range linuxBrowsers {
		if bin, err := exec.LookPath(name); err == nil {
			cache, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			profile := filepath.Join(cache, filepath.FromSlash(profileDir))
			return start(bin, "", append(appArgs, "--user-data-dir="+profile)...)
		}
	}
	for _, name := range urlOpeners {
		if bin, err := exec.LookPath(name); err == nil {
			return start(bin, "", url)
		}
	}
	return errors.New("no browser found; open the URL manually")
}

// WindowTitle ends every page title, which is also the app window's title.
const WindowTitle = "Skillboard"

// ErrNoWindow means Focus found no app window to bring forward.
var ErrNoWindow = errors.New("no app window found")

// The app window runs in its own browser process (its own profile), whose main window
// title is the page title. AppActivate does not restore a minimized window, and
// restoring one that is not minimized would un-maximize it, hence IsIconic.
const focusScript = `
$p = Get-Process brave, chrome, msedge -ErrorAction SilentlyContinue |
  Where-Object { $_.MainWindowTitle -like '*%s' } | Select-Object -First 1
if (-not $p) { exit 2 }
Add-Type -Namespace Skillboard -Name User32 -MemberDefinition @"
[DllImport("user32.dll")] public static extern bool IsIconic(System.IntPtr h);
[DllImport("user32.dll")] public static extern bool ShowWindow(System.IntPtr h, int cmd);
"@
if ([Skillboard.User32]::IsIconic($p.MainWindowHandle)) { [Skillboard.User32]::ShowWindow($p.MainWindowHandle, 9) | Out-Null }
if (-not (New-Object -ComObject WScript.Shell).AppActivate($p.Id)) { exit 3 }
`

// Focus brings the open app window to the front. Only Windows browsers under WSL are
// supported; elsewhere it returns errors.ErrUnsupported.
func Focus() error {
	if os.Getenv("WSL_DISTRO_NAME") == "" {
		return errors.ErrUnsupported
	}
	// An encoded command survives the trip through the Windows command line unquoted.
	script := utf16.Encode([]rune(fmt.Sprintf(focusScript, WindowTitle)))
	raw := make([]byte, 2*len(script))
	for i, u := range script {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	cmd.Dir = windowsDir
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		return ErrNoWindow
	case err != nil:
		return fmt.Errorf("focusing the window: %w: %s", err, out)
	}
	slog.Info("focused app window")
	return nil
}

// A Windows working directory avoids the "UNC paths are not supported" warning.
const windowsDir = "/mnt/c"

func windowsLocalAppData() (string, error) {
	// /u prints UTF-16LE; the default OEM code page would garble a non-ASCII user name.
	cmd := exec.Command("cmd.exe", "/u", "/c", "echo %LOCALAPPDATA%")
	cmd.Dir = windowsDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading %%LOCALAPPDATA%%: %w", err)
	}
	if len(out)%2 != 0 {
		return "", fmt.Errorf("reading %%LOCALAPPDATA%%: odd-length UTF-16 output %q", out)
	}
	units := make([]uint16, len(out)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(out[2*i:])
	}
	dir := strings.TrimSpace(string(utf16.Decode(units)))
	if dir == "" || strings.Contains(dir, "%") {
		return "", errors.New("%LOCALAPPDATA% is not set")
	}
	return dir, nil
}

func start(bin, dir string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return err
	}
	slog.Info("opened browser", "bin", bin)
	go cmd.Wait() //nolint:errcheck // only reaps the short-lived launcher process
	return nil
}
