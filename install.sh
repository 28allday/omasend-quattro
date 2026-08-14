#!/usr/bin/env bash
#
# Omasend installer — native Omarchy quattro shell plugin + engine.
#
#   curl -fsSL https://raw.githubusercontent.com/28allday/omasend-quattro/main/install.sh | bash
#
# What it does:
#   1. Installs the omasend-engine binary to ~/.local/bin (built from source
#      when run inside a clone with Go available, otherwise downloaded from
#      the latest GitHub release).
#   2. On an Omarchy quattro desktop (omarchy-shell present): registers the
#      shell plugin (omarchy plugin add + enable), installs the paper-plane
#      icon, and adds the Nautilus right-click "Send via Omasend" entry.
#   3. On anything else: engine only, with a note — the UI lives in the
#      shell, so a non-quattro box wants the original omarchy-send TUI
#      instead.
#
# Overrides:
#   BIN_DIR=~/bin            install the engine somewhere else
#   OMASEND_VERSION=v0.1.0   pin a release instead of latest
#   OMASEND_REPO=user/repo   download/register from a different repo
set -euo pipefail

REPO="${OMASEND_REPO:-28allday/omasend-quattro}"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
BIN="$BIN_DIR/omasend-engine"
PLUGIN_ID="nosignal.omasend"

# Resolve the clone dir when run from one (empty when curl-piped).
SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  [ -f "$SCRIPT_DIR/go.mod" ] || SCRIPT_DIR=""
fi

say() { printf '%s\n' "$*"; }

# ---- 1. engine binary ------------------------------------------------------
mkdir -p "$BIN_DIR"

if [ -n "$SCRIPT_DIR" ] && command -v go >/dev/null 2>&1; then
  say "Building omasend-engine from source…"
  (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o "$BIN" ./cmd/omasend-engine)
else
  arch="$(uname -m)"
  case "$arch" in
    x86_64) asset="omasend-engine-linux-amd64" ;;
    aarch64) asset="omasend-engine-linux-arm64" ;;
    *) say "Unsupported architecture: $arch (build from a clone with Go instead)"; exit 1 ;;
  esac
  if [ -n "${OMASEND_VERSION:-}" ]; then
    url="https://github.com/$REPO/releases/download/$OMASEND_VERSION/$asset"
  else
    url="https://github.com/$REPO/releases/latest/download/$asset"
  fi
  say "Downloading $url…"
  curl -fsSL -o "$BIN.tmp" "$url"
  chmod 755 "$BIN.tmp"
  mv "$BIN.tmp" "$BIN"
fi
say "Engine installed: $BIN"

# ---- 2. Omarchy quattro desktop integration --------------------------------
if ! command -v omarchy-shell >/dev/null 2>&1 \
   && [ ! -x /usr/share/omarchy/bin/omarchy-shell ]; then
  say ""
  say "No omarchy-shell found — engine installed, but the Omasend UI is an"
  say "omarchy-shell (Omarchy 4) plugin and cannot run here. On a headless"
  say "or pre-quattro box, use the original omarchy-send TUI instead:"
  say "  curl -fsSL https://raw.githubusercontent.com/28allday/omarchy-send/main/install.sh | bash"
  exit 0
fi

OMARCHY="omarchy"
command -v omarchy >/dev/null 2>&1 || OMARCHY="/usr/share/omarchy/bin/omarchy"

# Register the plugin. An existing install (including a dev symlink) is left
# alone — `omarchy plugin update` handles updates.
if [ ! -e "$HOME/.config/omarchy/plugins/$PLUGIN_ID" ]; then
  say "Registering the shell plugin…"
  "$OMARCHY" plugin add "https://github.com/$REPO.git" --yes
fi
"$OMARCHY" plugin enable "$PLUGIN_ID" right 2>/dev/null \
  || say "Enable it from Setup › Plugins (or: omarchy plugin enable $PLUGIN_ID)"

# ---- 3. icon ---------------------------------------------------------------
# Embedded so the curl-piped install has nothing extra to fetch.
icon_dir="$HOME/.local/share/icons/hicolor/scalable/apps"
mkdir -p "$icon_dir"
cat > "$icon_dir/omasend.svg" <<'SVG'
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256" width="256" height="256">
  <rect width="256" height="256" rx="56" fill="#16161e"/>
  <!-- paper-plane: upper wing (light) + lower flap (darker) -->
  <path d="M214 42 L42 114 L110 142 Z" fill="#7aa2f7"/>
  <path d="M214 42 L110 142 L130 214 L158 166 Z" fill="#5a7fd6"/>
</svg>
SVG
gtk-update-icon-cache -q -t -f "$HOME/.local/share/icons/hicolor" 2>/dev/null || true

# ---- 4. Nautilus right-click ----------------------------------------------
# Desktop-only nicety: "Send via Omasend" summons the panel with the selected
# files staged. Ships from the clone when available, otherwise from the
# embedded copy below (KEEP IN SYNC with nautilus/omasend.py).
if command -v nautilus >/dev/null 2>&1; then
  ext_dir="$HOME/.local/share/nautilus-python/extensions"
  mkdir -p "$ext_dir"
  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/nautilus/omasend.py" ]; then
    cp "$SCRIPT_DIR/nautilus/omasend.py" "$ext_dir/omasend.py"
  else
    cat > "$ext_dir/omasend.py" <<'PYEOF'
"""Nautilus right-click integration for Omasend.

Adds a "Send via Omasend" entry to the file/folder context menu. Omasend is
a native omarchy-shell plugin, so the entry summons its panel with the
selected paths staged as the summon payload — pick a device, press Enter,
and the engine sends. No terminal involved.

Both files and directories are supported (the engine expands a folder on
send).

Note on resolution: the Nautilus process inherits the graphical session's
PATH, which may not include Omarchy's bin dir, so omarchy-shell is resolved
to an absolute path — PATH first, then the known install locations.

Installed to ~/.local/share/nautilus-python/extensions/ by omasend's
install.sh on Omarchy desktops.
"""

import json
import os
import shutil

from gi import require_version

require_version("Nautilus", "4.1")

from gi.repository import GObject, Gio, Nautilus

PLUGIN_ID = "nosignal.omasend"


def _resolve(name, fallbacks):
    """Find an executable by PATH, then by a list of absolute fallback paths."""
    found = shutil.which(name)
    if found:
        return found
    for path in fallbacks:
        if path and os.path.isfile(path) and os.access(path, os.X_OK):
            return path
    return None


def _shell_binary():
    home = os.path.expanduser("~")
    fallbacks = [
        "/usr/share/omarchy/bin/omarchy-shell",
        os.path.join(home, ".local", "share", "omarchy", "bin", "omarchy-shell"),
    ]
    return _resolve("omarchy-shell", fallbacks)


class OmasendAction(GObject.GObject, Nautilus.MenuProvider):
    def _launch(self, paths):
        shell = _shell_binary()
        if not shell:
            return
        payload = json.dumps({"paths": paths})
        Gio.Subprocess.new(
            [shell, "shell", "summon", PLUGIN_ID, payload],
            Gio.SubprocessFlags.NONE,
        )

    def _selected_paths(self, files):
        paths = []
        seen = set()
        for file in files:
            location = file.get_location()
            if not location:
                continue
            path = location.get_path()
            if path and path not in seen:
                seen.add(path)
                paths.append(path)
        return paths

    def _make_item(self, paths):
        label = (
            "Send via Omasend"
            if len(paths) == 1
            else f"Send {len(paths)} items via Omasend"
        )
        item = Nautilus.MenuItem(
            name="OmasendNautilus::send",
            label=label,
            icon="omasend",
        )
        item.connect("activate", self._on_activate, paths)
        return item

    def _on_activate(self, _menu, paths):
        self._launch(paths)

    def get_file_items(self, *args):
        files = args[0] if len(args) == 1 else args[1]
        if not _shell_binary():
            return []
        paths = self._selected_paths(files)
        if not paths:
            return []
        return [self._make_item(paths)]
PYEOF
  fi
  nautilus -q >/dev/null 2>&1 || true
  say "Nautilus right-click installed (Send via Omasend)."
fi

say ""
say "Done. The bar icon lives in the right section; toggle the panel with:"
say "  omarchy-shell shell toggle $PLUGIN_ID"
