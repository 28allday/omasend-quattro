# Omasend

Send and receive files and messages between your Omarchy desktop and anything
running [LocalSend](https://localsend.org) — phones, laptops, other Linux
boxes — without opening an app. Omasend is a **native plugin for the Omarchy 4
desktop shell** (`omarchy-shell`): a paper-plane icon in the bar, a panel for
devices, messages and transfers, and an always-on receiver that runs quietly
with the shell.

It speaks the LocalSend v2.1 protocol, including the default encrypted (HTTPS)
mode, so stock LocalSend apps see it as just another device.

<p align="center">
  <img src="docs/devices.png" width="32%" alt="Devices tab">
  <img src="docs/staged.png" width="32%" alt="Files staged for sending">
  <img src="docs/offer.png" width="32%" alt="Incoming file offer">
</p>

## Quick start

On an Omarchy 4 desktop, add the plugin the standard way:

```sh
omarchy plugin add https://github.com/28allday/omasend-quattro.git --enable
```

Click the paper-plane icon in the bar to open the panel. Transfers are handled
by a small companion binary — the engine — which a plugin install can't place
on its own, so the first time you open the panel head to **Settings › Engine**
and press **Set up engine**. That builds it from the source in the plugin's own
checkout, or fetches the release pinned in `manifest.json` and checks its
SHA-256 against `engine/SHA256SUMS` before installing it. Either way you get
exactly the code you installed — nothing unpinned is pulled in.

Prefer the terminal? The same script the button runs:

```sh
~/.config/omarchy/plugins/nosignal.omasend/bin/omasend-setup
```

There's also `install.sh` in the repo, which does the above plus the optional
extras — the bar icon, the Nautilus right-click entry and zenity. Run it from a
clone:

```sh
git clone https://github.com/28allday/omasend-quattro.git
cd omasend-quattro && bash install.sh
```

Then, on the devices you want to talk to:

- **Phone or tablet** — install the free [LocalSend app](https://localsend.org)
  (iOS / Android). On the same Wi-Fi, it and your desktop find each other
  automatically.
- **Another desktop or laptop** — the official LocalSend app, or Omasend on
  another Omarchy 4 box.
- **A headless server** — the
  [omarchy-send](https://github.com/28allday/omarchy-send) terminal client.

Send something from the other device and it appears as an offer in the panel;
accept it and it lands in `~/Omasend`. That's it.

## Highlights

- **Always receiving** — if the desktop is running, the box can receive. Files
  land in `~/Omasend`, and a desktop notification tells you when they arrive.
- **Bar icon** — a paper plane in the bar; it badges on unread messages and
  active transfers, and a click opens the panel.
- **Send anything** — pick a device and press `f` for files or `Shift+F` for a
  whole folder (sent recursively, structure preserved), or right-click files
  in Nautilus → **Send via Omasend** to open the panel with your selection
  already staged.
- **Messages** — LocalSend-compatible plain-text messages in both directions:
  compose from a device row, read in the Messages tab.
- **You stay in control** — incoming offers show an accept/decline strip
  (auto-accept is optional), and an optional PIN gates who can send to you.
- **Finds devices anywhere** — multicast discovery on the LAN, automatic
  [Tailscale](https://tailscale.com) peers, and manual add-by-host/IP for
  everything else.
- **Scriptable** — send files or messages from scripts and AI agents through
  the shell's IPC, no UI involved.

## Requirements

Omarchy 4 with `omarchy-shell` — the UI is a shell plugin. On a headless or
pre-quattro box the panel can't run; use the original
[omarchy-send](https://github.com/28allday/omarchy-send) terminal client
there instead. Both speak the same protocol and happily talk to each other.

External dependencies: `jq` (already in the Omarchy base install) and
[zenity](https://gitlab.gnome.org/GNOME/zenity) for the panel's graphical
file chooser — the installer offers to add zenity if it's missing. Nothing
else; the engine is a single static binary with no runtime dependencies.

## Install details

**Why there are two pieces.** The UI is pure QML and loads straight into the
shell. The LocalSend protocol work — discovery, HTTPS transfers, PIN — lives in
`omasend-engine`, a small Go daemon, because the shell can't load native code.
`omarchy plugin add` installs the QML; the engine is a separate binary, so
`bin/omasend-setup` places it.

**Where the engine comes from.** `bin/omasend-setup` ships inside the plugin
and only ever installs from the checkout it sits in:

- **With Go installed** it compiles `./cmd/omasend-engine` from the source
  beside it. Nothing is downloaded, so the binary is precisely the commit you
  installed.
- **Without Go** it downloads the release asset for the version pinned in
  `manifest.json` — never `latest` — and refuses to install unless the SHA-256
  matches `engine/SHA256SUMS`, which is committed in the repo. A mismatch
  aborts and installs nothing.

Overrides: `BIN_DIR=…` puts the engine somewhere else, `OMASEND_REPO=user/repo`
uses a fork's releases. `bin/omasend-setup --check` reports what's installed
without changing anything.

**What `install.sh` adds.** Everything above plus the optional desktop extras:
the bar icon, the Nautilus right-click entry (copied from the checkout), and
[zenity](https://gitlab.gnome.org/GNOME/zenity) if it's missing (asks for
sudo) — the graphical chooser behind the panel's "Send file/folder" buttons;
without it they fall back to a typed-path prompt. It's safe to re-run, and
`omarchy plugin update` keeps the plugin itself current.

**Coming from omarchy-send?** Your identity survives: the first run migrates
`~/.config/omarchy-send/config.json` — alias, PIN, receive folder and the TLS
certificate — so devices that already paired with you stay paired. The old TUI
and Omasend can't run at the same time on one machine (both need port 53317).

## Using it

Click the paper plane in the bar, or from a terminal:

```sh
omarchy-shell shell toggle nosignal.omasend
```

The panel is fully keyboard-driven:

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | Move through the list |
| `Enter` / `m` | Message the selected device (sends staged items instead, if any are staged) |
| `f` / `Shift+F` | Pick files / a folder to send to the selected device |
| `+` | Add a remote device by host or IP |
| `a` / `d` | Accept / decline an incoming offer |
| `x` | Clear staged items |
| `1`–`4` / `Tab` | Switch tabs |
| `Esc` / `q` | Close the panel |

Every row also works with the mouse — the icons on a device row send a
message, files, or a folder directly. A single click only selects a device;
**double-click** it to send whatever you have staged (or to open a message
when nothing is staged).

### Receiving

Anything sent to you lands in `~/Omasend`. Files still on their way carry a
`.part` suffix until they're complete. With auto-accept off (the default),
each incoming batch shows an offer strip at the top of the panel — accept or
decline it; unattended offers simply expire. Flip **Auto-accept** in Settings
if you'd rather everything just arrives.

Set a **PIN** in Settings and senders must enter it before anything reaches
you — the same PIN mechanism the official LocalSend apps use.

**Files arriving but no notification?** Check Do Not Disturb — with DND on,
the shell still accepts every notification and files it into history, it just
never shows one, so arrivals land silently in `~/Omasend`. Run
`omarchy-shell notifications dndState` to check, `setDnd off` to turn it off,
and `showHistory` to replay what you missed.

### Messages

Incoming messages raise a notification and badge the bar icon; the Messages
tab keeps the recent conversation (messages live in the panel, not on disk).
Press `Enter` on a device to reply.

### Transfers

The Transfers tab shows live progress for everything moving in either
direction. Dismiss finished rows individually, or press `c` to clear all
finished transfers.

### Right-click in Nautilus

Select files or folders in Nautilus, right-click → **Send via Omasend**. The
panel opens with your selection staged — pick a device, press `Enter`, done.

### Remote devices

Multicast discovery only reaches your own LAN. For everything else: online
Tailscale peers are discovered automatically when Tailscale is running, and
`+` adds any device by host or IP, remembered for next time.

### From scripts and agents

The shell exposes Omasend over IPC, so anything on the box can send without a
UI or a TTY:

```sh
omarchy-shell omasend send "phone" "on my way"          # message
omarchy-shell omasend sendFile "phone" "/path/to/file"  # file or folder
omarchy-shell omasend status                            # engine health
```

Sends are queued and delivered asynchronously; a failure raises a desktop
notification. The installer also writes a managed block into
`~/.claude/CLAUDE.md` so AI agents on the machine know how to use it.

## Under the hood

Two parts: a pure-QML shell plugin (bar widget, panel, service) and
`omasend-engine`, a small Go daemon that speaks the LocalSend protocol —
discovery, HTTPS transfers, PIN, the lot. The shell starts and supervises the
engine and talks to it over a local socket; nothing listens beyond LocalSend's
standard port `53317` (TCP + UDP). Configuration lives in
`~/.config/omasend/config.json`. If you run a firewall, allow `53317` for both
TCP and UDP on your LAN.

## What it installs and writes

Everything lands under your home directory — nothing is written system-wide.
`omarchy plugin add` writes only the first two rows; the rest arrive when you
set up the engine, run `install.sh`, or start receiving:

| Path | What goes there |
| --- | --- |
| `~/.config/omarchy/plugins/nosignal.omasend/` | the plugin itself, cloned by `omarchy plugin add` |
| `~/.config/omarchy/shell.json` | a `plugins[]` entry and a bar-layout entry, so the icon and panel load |
| `~/.local/bin/omasend-engine` | the transfer engine, placed by `bin/omasend-setup` |
| `~/.config/omasend/config.json` | your alias, PIN, receive folder, known remotes and TLS certificate |
| `~/Omasend/` | received files (configurable in Settings) |
| `~/.local/share/icons/hicolor/scalable/apps/` | the paper-plane icon |
| `~/.local/share/nautilus-python/extensions/omasend.py` | the "Send via Omasend" right-click entry |
| `~/.claude/CLAUDE.md` | a delimited managed block telling AI agents how to send from scripts |

Two of those are edits to files you may already own, so to be explicit about
them: the `shell.json` entries are added with `jq` and leave the rest of the
file untouched (the plugin re-adds its own `plugins[]` reference on first open
if the shell dropped it — without that, removing the bar icon would kill the
panel too); and the `CLAUDE.md` block sits between
`<!-- BEGIN omasend -->` / `<!-- END omasend -->` markers, so re-running the
installer replaces only that block and never the surrounding file. Neither is
overwritten wholesale.

The one privileged step is installing zenity — `sudo pacman -S zenity`, only
if it isn't already present, and it's non-fatal if you decline. The engine
listens on LocalSend's standard port `53317` (TCP + UDP) whenever the shell is
running; that's the whole point of always-on receiving, and it's the only
network listener.

## Uninstall

```sh
omarchy plugin remove nosignal.omasend
rm ~/.local/bin/omasend-engine
rm ~/.local/share/nautilus-python/extensions/omasend.py
rm ~/.local/share/icons/hicolor/scalable/apps/omasend.svg
rm -rf ~/.config/omasend            # alias, PIN, pairing certificate
```

That leaves received files in `~/Omasend` alone. The managed block in
`~/.claude/CLAUDE.md` is delimited by its `BEGIN`/`END` markers if you want to
delete it by hand.

## License

MIT — see [LICENSE](LICENSE). Omasend is an independent project, not
affiliated with or endorsed by LocalSend; it merely speaks the same protocol.
