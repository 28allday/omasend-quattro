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
