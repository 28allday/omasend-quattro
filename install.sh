#!/usr/bin/env bash
#
# Omasend installer — native Omarchy quattro shell plugin + engine.
#
# Run it from a clone (bash install.sh), or use the one-line install command
# documented in the README.
#
# What it does:
#   1. Installs the omasend-engine binary to ~/.local/bin (built from source
#      when run inside a clone with Go available, otherwise downloaded from
#      the latest GitHub release).
#   2. On an Omarchy quattro desktop (omarchy-shell present): registers the
#      shell plugin (omarchy plugin add + enable), installs zenity if missing
#      (the panel's file chooser), the paper-plane icon, and the Nautilus
#      right-click "Send via Omasend" entry.
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
  say "  https://github.com/28allday/omarchy-send"
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
# A previous install leaves a stale plugins[] self-reference in shell.json
# that makes `plugin enable` report ok WITHOUT placing the bar widget.
# Disabling first clears it (harmless when the plugin was never enabled);
# skip when already enabled so a re-run doesn't move a user-placed widget.
status="$("$OMARCHY" plugin list 2>/dev/null | awk -v id="$PLUGIN_ID" '$1==id{print $2}')"
if [ "$status" != "enabled" ]; then
  "$OMARCHY" plugin disable "$PLUGIN_ID" >/dev/null 2>&1 || true
fi
"$OMARCHY" plugin enable "$PLUGIN_ID" right 2>/dev/null \
  || say "Enable it from Setup › Plugins (or: omarchy plugin enable $PLUGIN_ID)"

# ---- 3. zenity (graphical file chooser) ------------------------------------
# The panel's "Send file/folder" buttons open a GTK chooser via zenity, which
# is NOT in the Omarchy base install (some boxes only have it as a Steam
# dependency). Without it the panel silently falls back to a typed-path
# prompt, so make sure it's really here. sudo reads from /dev/tty so the
# password prompt still works under a piped install; failure is non-fatal.
if ! command -v zenity >/dev/null 2>&1; then
  say "Installing zenity (file chooser for panel sends — needs sudo)…"
  # shellcheck disable=SC2024  # stdin redirect is for sudo's own password prompt
  if sudo pacman -S --needed --noconfirm zenity </dev/tty; then
    say "zenity installed."
  else
    say "Could not install zenity — 'Send file/folder' will fall back to a"
    say "typed-path prompt until you run: sudo pacman -S zenity"
  fi
fi

# ---- 4. icon ---------------------------------------------------------------
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

# ---- 5. Nautilus right-click ----------------------------------------------
# Desktop-only nicety: "Send via Omasend" summons the panel with the selected
# files staged. Ships from the clone when available, otherwise fetched from
# the repo — one source of truth, no embedded copy to drift.
if command -v nautilus >/dev/null 2>&1; then
  ext_dir="$HOME/.local/share/nautilus-python/extensions"
  mkdir -p "$ext_dir"
  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/nautilus/omasend.py" ]; then
    cp "$SCRIPT_DIR/nautilus/omasend.py" "$ext_dir/omasend.py"
    say "Nautilus right-click installed (Send via Omasend)."
  else
    ext_url="https://raw.githubusercontent.com/$REPO/main/nautilus/omasend.py"
    if curl -fsSL -o "$ext_dir/omasend.py.tmp" "$ext_url"; then
      mv "$ext_dir/omasend.py.tmp" "$ext_dir/omasend.py"
      say "Nautilus right-click installed (Send via Omasend)."
    else
      rm -f "$ext_dir/omasend.py.tmp"
      say "Could not fetch $ext_url — skipped the Nautilus right-click entry."
    fi
  fi
  nautilus -q >/dev/null 2>&1 || true
fi

# ---- 6. agent context ------------------------------------------------------
# Teach AI agents on this box what Omasend is and how to send from scripts.
# Idempotent: an existing managed block is replaced, other content is kept.
claude_md="$HOME/.claude/CLAUDE.md"
mkdir -p "$HOME/.claude"
recv_dir="$HOME/Omasend"
cfg="$HOME/.config/omasend/config.json"
if [ -f "$cfg" ] && command -v jq >/dev/null 2>&1; then
  d="$(jq -r '.receiveDir // empty' "$cfg" 2>/dev/null || true)"
  [ -n "$d" ] && recv_dir="$d"
fi
if [ -f "$claude_md" ]; then
  sed -i '/<!-- BEGIN omasend (managed by installer) -->/,/<!-- END omasend (managed by installer) -->/d' "$claude_md"
fi
cat >> "$claude_md" <<AGENTEOF

<!-- BEGIN omasend (managed by installer) -->
## Omasend (installed on this machine)

Omasend is the LocalSend-compatible file-transfer plugin for
omarchy-shell (Omarchy 4). It is the successor to the omarchy-send
TUI — if that is also installed here, never run it while the shell is
up: only one process can bind port 53317, and Omasend's engine
(\`~/.local/bin/omasend-engine\`, auto-managed by the shell) is the
receiver. Other devices send files/messages to this box over
LAN/Tailscale (TCP 53317). **Files sent here land in \`$recv_dir\`**;
files still transferring carry a \`.part\` suffix — skip them.
Receiving works whenever the desktop shell is running; incoming
messages appear in the panel's Messages tab, not on disk.

Agents/scripts can SEND from this box via shell IPC (no TTY):
\`omarchy-shell omasend send "<alias>" "<text>"\` for a message, and
\`omarchy-shell omasend sendFile "<alias>" "<absolute path>"\` for a
file or folder (one path per call; folders send whole). **"OSF" is
user shorthand** — "OSF <file> to <alias>" means run that sendFile
IPC. Replies are "queued seq N" — delivery is async; a failure raises
a desktop notification. \`omarchy-shell omasend status\` shows engine
health. Panel UI: \`omarchy-shell shell toggle $PLUGIN_ID\`.
<!-- END omasend (managed by installer) -->
AGENTEOF
say "Agent context written to $claude_md."

say ""
say "Done. The bar icon lives in the right section; toggle the panel with:"
say "  omarchy-shell shell toggle $PLUGIN_ID"
