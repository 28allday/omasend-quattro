import Quickshell
import Quickshell.Io
import Quickshell.Wayland
import QtQuick
import qs.Commons
import qs.Ui

// Omasend panel. Summoned/toggled through the shell host:
//   omarchy-shell shell toggle nosignal.omasend
// The host calls open(payloadJson) / close() and reads `opened`; it also
// injects `shell` right after the Loader resolves (see onShellChanged).
//
// All live state (peers, messages, transfers, offers, settings) lives in the
// sibling Service.qml instance — reached via shell.serviceFor() — which owns
// the omasend-engine daemon. This file is presentation + input only: nothing
// here talks to the engine socket directly, so the panel can be destroyed and
// rebuilt freely while transfers keep running.
//
// Layout: header (identity + engine state), an urgent offers strip when a
// peer is waiting for accept/decline, then four tabs — Devices, Messages,
// Transfers, Settings — with single-key navigation, and one modal input used
// for compose / file paths / PIN entry.
Item {
  id: root

  property bool opened: false

  readonly property string selfId: "nosignal.omasend"

  // Injected by the shell host after the Loader resolves.
  property var shell: null
  onShellChanged: {
    if (!root.opened && root.shell && root.shell.openPanelIds
        && root.shell.openPanelIds[root.selfId] === true)
      root.open("{}")
  }

  readonly property var svc: (root.shell && typeof root.shell.serviceFor === "function")
                             ? root.shell.serviceFor(root.selfId) : null
  readonly property bool hasService: root.svc !== null && root.svc !== undefined

  // The service lands after this panel's Component.onCompleted, so seed the
  // unbound settings fields whenever it (re)appears.
  onSvcChanged: {
    if (!root.hasService) return
    if (!aliasField.activeFocus) aliasField.text = root.svc.alias
    if (!dirField.activeFocus) dirField.text = root.svc.receiveDir
  }

  // ------------------------------------------------------------------ theme
  property color background: Color.menu.background
  property color foreground: Color.menu.text
  property color border: Color.menu.border
  property var borderSpec: Border.surfaceSpec("menu", "border", border, Math.max(1, Style.space(2)))
  property color selBg: Color.menu.selectedBackground
  property color selText: Color.menu.selectedText
  property color accent: Color.accent
  property color urgent: Color.urgent
  property color good: Color.accent
  readonly property int cornerRadius: Style.cornerRadius
  property string fontFamily: Style.font.menuFamily
  property int contentMargin: Style.spacing.panelPadding
  property int sectionGap: Style.spacing.xl
  readonly property int rowH: Style.font.title + Style.spacing.controlPaddingY * 2 + Style.spacing.md
  readonly property int maxVisibleRows: 9

  // ------------------------------------------------------------------ state
  property int tab: 0            // 0 devices, 1 messages, 2 transfers, 3 settings
  readonly property var tabNames: ["Devices", "Messages", "Transfers", "Settings"]
  property int selectedIndex: -1

  // One modal input reused for everything that needs typing.
  // mode: "" (closed) | "message" | "files" | "pin" | "addpeer"
  property string modalMode: ""
  property var modalPeer: null      // peer the compose/file send targets
  property string statusLine: ""    // transient feedback under the header

  // The last send attempt, kept so a PIN challenge can retry it.
  property var pendingSend: null    // {kind:"message"|"files", to, ip, text, paths}
  property int awaitSeq: -1

  // Paths staged by a summon payload ({"paths":[...]} — the Nautilus
  // right-click route): pick a device and Enter sends them via the engine.
  property var stagedPaths: []

  // ------------------------------------------------- self-reference (bar fix)
  // `omarchy plugin enable` writes only the bar.layout entry for a
  // bar-widget+panel plugin; if the bar icon is later removed the shell finds
  // no reference, stops instantiating the panel, and the keybinding dies.
  // First open claims a plugins[] reference of our own. Idempotent; inert
  // once the shell writes both references itself.
  property bool selfRefEnsured: false
  readonly property string ensureSelfRefScript: [
    'id="$1"',
    'f="$HOME/.config/omarchy/shell.json"',
    '[ -f "$f" ] || exit 0',
    'jq -e --arg id "$id" \'any(.plugins[]?; (.id // empty) == $id)\' "$f" >/dev/null && exit 0',
    'tmp="$f.selfref.$$"',
    'jq --arg id "$id" \'.plugins = ((.plugins // []) + [{id: $id}])\' "$f" > "$tmp" || {',
    '  rm -f "$tmp"; exit 1;',
    '}',
    '[ -s "$tmp" ] || { rm -f "$tmp"; exit 1; }',
    'mv "$tmp" "$f"'
  ].join("\n")

  function ensureSelfReference() {
    if (root.selfRefEnsured) return
    root.selfRefEnsured = true
    Quickshell.execDetached(["sh", "-c", root.ensureSelfRefScript, "plugin-selfref", root.selfId])
  }

  // ------------------------------------------------------------- open/close
  function open(payloadJson) {
    root.opened = true
    root.ensureSelfReference()
    root.selectedIndex = -1
    root.statusLine = ""
    var staged = []
    try {
      var p = JSON.parse(String(payloadJson || "{}"))
      if (p && Array.isArray(p.paths))
        staged = p.paths.filter(function(x) { return String(x).trim() !== "" })
    } catch (e) {}
    root.stagedPaths = staged
    if (staged.length > 0) {
      root.tab = 0
      root.selectedIndex = root.peers.length > 0 ? 0 : -1
    } else if (root.hasService && root.svc.offers.length > 0) {
      root.tab = 0
    }
    Qt.callLater(function() { keyCatcher.forceActiveFocus() })
  }

  function sendStaged(peer) {
    if (!root.hasService || root.stagedPaths.length === 0 || !peer) return
    root.pendingSend = { kind: "files", to: peer.alias, ip: peer.ip, paths: root.stagedPaths.slice() }
    root.awaitSeq = root.svc.sendFiles("", peer.ip, root.stagedPaths, "")
    root.statusLine = "Sending " + root.stagedPaths.length + " item(s) to " + peer.alias + "…"
  }

  // "Send files/folder" opens the GTK chooser via the service (the panel
  // closes so the dialog isn't under a keyboard-exclusive overlay); the
  // typed-path modal remains only as the no-zenity fallback.
  function pickFiles(peer, wantDir) {
    if (!peer) return
    if (root.hasService && root.svc.pickAndSend(peer, wantDir === true)) {
      root.close()
      return
    }
    root.openModal("files", peer)
  }

  function close() {
    if (!root.opened) return
    root.opened = false
    root.closeModal()
    if (root.shell && typeof root.shell.hide === "function")
      root.shell.hide(root.selfId)
  }

  function toggle() {
    if (root.opened) root.close()
    else root.open("{}")
  }

  // ------------------------------------------------------------- formatting
  function humanBytes(n) {
    n = Number(n) || 0
    if (n < 1024) return n + " B"
    var units = ["KB", "MB", "GB", "TB"]
    var v = n
    for (var i = 0; i < units.length; i++) {
      v = v / 1024
      if (v < 1024 || i === units.length - 1)
        return (v < 10 ? v.toFixed(1) : Math.round(v)) + " " + units[i]
    }
    return n + " B"
  }

  function timeLabel(iso) {
    var d = new Date(String(iso || ""))
    if (isNaN(d.getTime())) return ""
    var now = new Date()
    var hm = ("0" + d.getHours()).slice(-2) + ":" + ("0" + d.getMinutes()).slice(-2)
    if (d.toDateString() === now.toDateString()) return hm
    return (d.getMonth() + 1) + "/" + d.getDate() + " " + hm
  }

  function deviceGlyph(type) {
    switch (String(type || "")) {
      case "mobile": return "󰄜"
      case "desktop": return "󰟀"
      case "laptop": return "󰌢"
      case "server": return "󰒋"
      case "web": return "󰖟"
      default: return "󰇄"
    }
  }

  // ------------------------------------------------------------------ lists
  readonly property var peers: root.hasService ? root.svc.peers : []
  readonly property var offers: root.hasService ? root.svc.offers : []
  readonly property var messages: root.hasService ? root.svc.messages : []
  readonly property var transfers: root.hasService ? root.svc.transfers : []

  readonly property int listLength: root.tab === 0 ? root.peers.length
                                   : root.tab === 2 ? root.transfers.length : 0

  function move(delta) {
    if (root.listLength === 0) { root.selectedIndex = -1; return }
    var i = root.selectedIndex + delta
    if (i < 0) i = 0
    if (i >= root.listLength) i = root.listLength - 1
    root.selectedIndex = i
  }

  function setTab(t) {
    if (t < 0) t = root.tabNames.length - 1
    if (t >= root.tabNames.length) t = 0
    root.tab = t
    root.selectedIndex = root.listLength > 0 ? 0 : -1
    if (t === 1 && root.hasService) root.svc.clearUnread()
  }

  // ------------------------------------------------------------------ modal
  function openModal(mode, peer) {
    root.modalMode = mode
    root.modalPeer = peer || null
    modalInput.text = ""
    Qt.callLater(function() { modalInput.forceActiveFocus() })
  }

  function closeModal() {
    root.modalMode = ""
    root.modalPeer = null
    modalInput.text = ""
    if (root.opened) Qt.callLater(function() { keyCatcher.forceActiveFocus() })
  }

  function modalTitle() {
    switch (root.modalMode) {
      case "message": return "Message to " + (root.modalPeer ? root.modalPeer.alias : "")
      case "files": return "Send file/folder to " + (root.modalPeer ? root.modalPeer.alias : "")
      case "pin": return "PIN required" + (root.pendingSend ? " for " + root.pendingSend.to : "")
      case "addpeer": return "Add remote device (host / IP / Tailscale name)"
      default: return ""
    }
  }

  function modalPlaceholder() {
    switch (root.modalMode) {
      case "message": return "Type a message…"
      case "files": return "~/path/to/file-or-folder"
      case "pin": return "PIN"
      case "addpeer": return "e.g. durden or 100.64.0.7"
      default: return ""
    }
  }

  function submitModal() {
    var text = String(modalInput.text || "").trim()
    if (text === "" || !root.hasService) { root.closeModal(); return }
    switch (root.modalMode) {
      case "message":
        root.pendingSend = { kind: "message", to: root.modalPeer.alias, ip: root.modalPeer.ip, text: text }
        root.awaitSeq = root.svc.sendMessage("", root.modalPeer.ip, text, "")
        root.statusLine = "Sending message to " + root.modalPeer.alias + "…"
        break
      case "files":
        root.pendingSend = { kind: "files", to: root.modalPeer.alias, ip: root.modalPeer.ip, paths: [text] }
        root.awaitSeq = root.svc.sendFiles("", root.modalPeer.ip, [text], "")
        root.statusLine = "Sending to " + root.modalPeer.alias + "…"
        break
      case "pin":
        if (root.pendingSend) {
          var p = root.pendingSend
          root.awaitSeq = (p.kind === "message")
            ? root.svc.sendMessage("", p.ip, p.text, text)
            : root.svc.sendFiles("", p.ip, p.paths, text)
          root.statusLine = "Retrying with PIN…"
        }
        break
      case "addpeer":
        root.svc.addPeer(text)
        root.statusLine = "Probing " + text + "…"
        break
    }
    root.closeModal()
  }

  // React to send outcomes (also drives the PIN challenge flow).
  Connections {
    target: root.svc
    enabled: root.hasService
    function onLastSendResultChanged() {
      var r = root.svc.lastSendResult
      if (!r || r.seq !== root.awaitSeq) return
      if (r.ok) {
        root.statusLine = "Sent to " + (r.to || (root.pendingSend ? root.pendingSend.to : "peer"))
        if (root.pendingSend && root.pendingSend.kind === "files")
          root.stagedPaths = []
        root.pendingSend = null
        root.awaitSeq = -1
      } else if (r.pinRequired) {
        root.statusLine = "Peer requires a PIN"
        if (root.opened) root.openModal("pin", null)
      } else {
        root.statusLine = "Send failed: " + r.error
        root.pendingSend = null
        root.awaitSeq = -1
      }
    }
  }

  // ------------------------------------------------------------------ window
  PanelWindow {
    id: panel
    visible: root.opened
    anchors { top: true; bottom: true; left: true; right: true }
    color: "transparent"
    WlrLayershell.namespace: "omarchy-omasend"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.keyboardFocus: root.opened ? WlrKeyboardFocus.Exclusive : WlrKeyboardFocus.None
    exclusionMode: ExclusionMode.Ignore

    MouseArea {
      anchors.fill: parent
      onClicked: root.close()
    }

    BorderSurface {
      id: card
      width: Math.min(Style.space(560), panel.width - Style.gapsOut * 2)
      height: Math.min(Style.space(620),
                       Math.min(panel.height * 0.85,
                                panel.height - Style.bar.sizeHorizontal - Style.gapsOut * 2))
      radius: root.cornerRadius
      anchors.top: parent.top
      anchors.right: parent.right
      anchors.topMargin: Style.bar.sizeHorizontal + Style.gapsOut
      anchors.rightMargin: Style.gapsOut
      color: root.background
      borderSpec: root.borderSpec
      padding: root.contentMargin
      clip: true

      MouseArea { anchors.fill: parent; onClicked: {} }

      Item {
        id: keyCatcher
        anchors.fill: parent
        focus: true
        Keys.onPressed: function(event) {
          if (root.modalMode !== "") {
            if (event.key === Qt.Key_Escape) { root.closeModal(); event.accepted = true }
            return
          }
          if (event.key === Qt.Key_Escape || event.key === Qt.Key_Q) {
            root.close(); event.accepted = true
          } else if (event.key === Qt.Key_1) { root.setTab(0); event.accepted = true
          } else if (event.key === Qt.Key_2) { root.setTab(1); event.accepted = true
          } else if (event.key === Qt.Key_3) { root.setTab(2); event.accepted = true
          } else if (event.key === Qt.Key_4) { root.setTab(3); event.accepted = true
          } else if (event.key === Qt.Key_Tab) { root.setTab(root.tab + 1); event.accepted = true
          } else if (event.key === Qt.Key_Backtab) { root.setTab(root.tab - 1); event.accepted = true
          } else if (event.key === Qt.Key_Up || event.key === Qt.Key_K) { root.move(-1); event.accepted = true
          } else if (event.key === Qt.Key_Down || event.key === Qt.Key_J) { root.move(1); event.accepted = true
          } else if (event.key === Qt.Key_A && root.offers.length > 0) {
            root.svc.acceptOffer(root.offers[0].offerId, true); event.accepted = true
          } else if (event.key === Qt.Key_D && root.offers.length > 0) {
            root.svc.acceptOffer(root.offers[0].offerId, false); event.accepted = true
          } else if (root.tab === 0) {
            var peer = (root.selectedIndex >= 0 && root.selectedIndex < root.peers.length)
                       ? root.peers[root.selectedIndex] : null
            if ((event.key === Qt.Key_Return || event.key === Qt.Key_Enter)
                && peer && root.stagedPaths.length > 0) {
              root.sendStaged(peer); event.accepted = true
            } else if (event.key === Qt.Key_X && root.stagedPaths.length > 0) {
              root.stagedPaths = []; root.statusLine = ""; event.accepted = true
            } else if ((event.key === Qt.Key_Return || event.key === Qt.Key_Enter
                 || event.key === Qt.Key_M) && peer) {
              root.openModal("message", peer); event.accepted = true
            } else if (event.key === Qt.Key_F && peer) {
              root.pickFiles(peer, (event.modifiers & Qt.ShiftModifier) !== 0)
              event.accepted = true
            } else if (event.key === Qt.Key_Plus) {
              root.openModal("addpeer", null); event.accepted = true
            }
          } else if (root.tab === 2 && event.key === Qt.Key_C) {
            if (root.hasService) root.svc.pruneTransfers()
            event.accepted = true
          }
        }
      }

      // BorderSurface does not inset children itself — its padding only feeds
      // the content*Inset helpers, which children apply as margins.
      Column {
        anchors.fill: parent
        anchors.topMargin: card.contentTopInset
        anchors.rightMargin: card.contentRightInset
        anchors.bottomMargin: card.contentBottomInset
        anchors.leftMargin: card.contentLeftInset
        spacing: root.sectionGap

        // ---------------------------------------------------------- header
        Item {
          width: parent.width
          height: Style.font.title + Style.spacing.lg

          Text {
            anchors.left: parent.left
            anchors.verticalCenter: parent.verticalCenter
            text: "󰒊  Omasend"
            color: root.accent
            font.family: root.fontFamily
            font.pixelSize: Style.font.title
            font.bold: true
          }

          Text {
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            text: {
              if (!root.hasService) return "no service"
              if (root.svc.engineMissing) return "engine not installed"
              if (!root.svc.connected) return "engine starting…"
              return root.svc.alias
            }
            color: root.hasService && root.svc.connected ? root.foreground : root.urgent
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }
        }

        // Transient status / hint line.
        Text {
          visible: root.statusLine !== ""
          width: parent.width
          text: root.statusLine
          color: Qt.darker(root.foreground, 1.3)
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          elide: Text.ElideRight
        }

        // ---------------------------------------------------------- offers
        Rectangle {
          visible: root.offers.length > 0
          width: parent.width
          height: visible ? offerCol.implicitHeight + Style.spacing.lg * 2 : 0
          radius: root.cornerRadius / 2
          color: Qt.rgba(root.urgent.r, root.urgent.g, root.urgent.b, 0.12)
          border.color: root.urgent
          border.width: 1

          Column {
            id: offerCol
            anchors.fill: parent
            anchors.margins: Style.spacing.lg
            spacing: Style.spacing.sm

            Repeater {
              model: root.offers
              delegate: Item {
                required property var modelData
                width: offerCol.width
                height: Style.font.body + Style.spacing.controlPaddingY * 2

                Text {
                  anchors.left: parent.left
                  anchors.right: offerButtons.left
                  anchors.rightMargin: Style.spacing.md
                  anchors.verticalCenter: parent.verticalCenter
                  text: modelData.from + " → " + (modelData.files.length === 1
                          ? modelData.files[0].name
                          : modelData.files.length + " files")
                        + "  (" + root.humanBytes(modelData.total) + ")"
                  color: root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  elide: Text.ElideMiddle
                }

                Row {
                  id: offerButtons
                  anchors.right: parent.right
                  anchors.verticalCenter: parent.verticalCenter
                  spacing: Style.spacing.sm

                  Button {
                    text: "Accept (a)"
                    accent: root.good
                    onClicked: root.svc.acceptOffer(modelData.offerId, true)
                  }
                  Button {
                    text: "Decline (d)"
                    accent: root.urgent
                    onClicked: root.svc.acceptOffer(modelData.offerId, false)
                  }
                }
              }
            }
          }
        }

        // ---------------------------------------------------------- staged
        // Files handed in by a summon payload (Nautilus right-click): shown
        // until they're sent to the picked device or cleared.
        Rectangle {
          visible: root.stagedPaths.length > 0
          width: parent.width
          height: visible ? stagedRow.implicitHeight + Style.spacing.lg * 2 : 0
          radius: root.cornerRadius / 2
          color: Qt.rgba(root.accent.r, root.accent.g, root.accent.b, 0.12)
          border.color: root.accent
          border.width: 1

          Item {
            id: stagedRow
            anchors.fill: parent
            anchors.margins: Style.spacing.lg
            implicitHeight: Math.max(stagedText.implicitHeight, stagedClear.implicitHeight)

            Column {
              id: stagedText
              anchors.left: parent.left
              anchors.right: stagedClear.left
              anchors.rightMargin: Style.spacing.md
              anchors.verticalCenter: parent.verticalCenter

              Text {
                width: parent.width
                text: "Send " + root.stagedPaths.length + " item(s) — pick a device, press Enter"
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                elide: Text.ElideRight
              }
              Text {
                width: parent.width
                text: root.stagedPaths.map(function(p) {
                  var s = String(p); var i = s.lastIndexOf("/")
                  return i >= 0 ? s.substring(i + 1) : s
                }).join(", ")
                color: Qt.darker(root.foreground, 1.3)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                elide: Text.ElideMiddle
              }
            }

            Button {
              id: stagedClear
              anchors.right: parent.right
              anchors.verticalCenter: parent.verticalCenter
              text: "Clear (x)"
              onClicked: { root.stagedPaths = []; root.statusLine = "" }
            }
          }
        }

        // ------------------------------------------------------------ tabs
        Row {
          width: parent.width
          spacing: Style.spacing.sm

          Repeater {
            model: root.tabNames
            delegate: Button {
              required property var modelData
              required property int index
              text: (index === 1 && root.hasService && root.svc.unreadMessages > 0)
                    ? modelData + " (" + root.svc.unreadMessages + ")"
                    : modelData
              selected: root.tab === index
              foreground: root.tab === index ? root.accent : root.foreground
              onClicked: root.setTab(index)
            }
          }
        }

        PanelSeparator { width: parent.width }

        // --------------------------------------------------------- content
        // Stops short of the footer's reserved strip at the card bottom.
        Item {
          width: parent.width
          height: parent.height - y - footer.height - root.sectionGap

          // ---- Devices ----
          ListView {
            visible: root.tab === 0
            anchors.fill: parent
            model: root.peers
            clip: true
            spacing: Style.spacing.xs

            delegate: Rectangle {
              required property var modelData
              required property int index
              width: ListView.view.width
              height: root.rowH
              radius: root.cornerRadius / 2
              color: root.selectedIndex === index && root.tab === 0 ? root.selBg : "transparent"

              MouseArea {
                anchors.fill: parent
                onClicked: root.selectedIndex = index
                onDoubleClicked: {
                  if (root.stagedPaths.length > 0) root.sendStaged(modelData)
                  else root.openModal("message", modelData)
                }
              }

              Text {
                id: devGlyph
                anchors.left: parent.left
                anchors.leftMargin: Style.spacing.md
                anchors.verticalCenter: parent.verticalCenter
                text: root.deviceGlyph(modelData.type)
                color: root.selectedIndex === index ? root.selText : root.accent
                font.family: root.fontFamily
                font.pixelSize: Style.font.icon
              }

              Column {
                anchors.left: devGlyph.right
                anchors.leftMargin: Style.spacing.lg
                anchors.right: devActions.left
                anchors.verticalCenter: parent.verticalCenter

                Text {
                  width: parent.width
                  text: modelData.alias
                  color: root.selectedIndex === index ? root.selText : root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  elide: Text.ElideRight
                }
                Text {
                  width: parent.width
                  text: (modelData.model ? modelData.model + " · " : "") + modelData.ip
                  color: Qt.darker(root.selectedIndex === index ? root.selText : root.foreground, 1.4)
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                  elide: Text.ElideRight
                }
              }

              Row {
                id: devActions
                anchors.right: parent.right
                anchors.rightMargin: Style.spacing.md
                anchors.verticalCenter: parent.verticalCenter
                spacing: Style.spacing.xs

                Button {
                  iconText: "󰍡"
                  tooltipText: "Send a message (m)"
                  onClicked: root.openModal("message", modelData)
                }
                Button {
                  iconText: "󰈙"
                  tooltipText: "Send files (f)"
                  onClicked: root.pickFiles(modelData, false)
                }
                Button {
                  iconText: "󰉋"
                  tooltipText: "Send a folder (shift+F)"
                  onClicked: root.pickFiles(modelData, true)
                }
              }
            }

            Text {
              visible: root.peers.length === 0
              anchors.centerIn: parent
              text: root.hasService && root.svc.connected
                    ? "No devices found yet.\nOpen LocalSend/Omasend on another device,\nor press + to add a remote host."
                    : "Waiting for the engine…"
              color: Qt.darker(root.foreground, 1.4)
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              horizontalAlignment: Text.AlignHCenter
            }
          }

          // ---- Messages ----
          ListView {
            id: msgList
            visible: root.tab === 1
            anchors.fill: parent
            model: root.messages
            clip: true
            spacing: Style.spacing.sm
            onCountChanged: positionViewAtEnd()
            onVisibleChanged: if (visible) positionViewAtEnd()

            delegate: Column {
              required property var modelData
              width: ListView.view.width

              Text {
                width: parent.width
                text: (modelData.outgoing ? "→ " + (modelData.to || "peer")
                                          : "← " + modelData.from)
                      + "   " + root.timeLabel(modelData.time)
                color: modelData.outgoing ? root.accent : Qt.darker(root.foreground, 1.3)
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
              }
              Text {
                width: parent.width
                text: modelData.text
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                wrapMode: Text.Wrap
              }
            }

            Text {
              visible: root.messages.length === 0
              anchors.centerIn: parent
              text: "No messages yet.\nPick a device and press Enter to send one."
              color: Qt.darker(root.foreground, 1.4)
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              horizontalAlignment: Text.AlignHCenter
            }
          }

          // ---- Transfers ----
          ListView {
            visible: root.tab === 2
            anchors.fill: parent
            model: root.transfers
            clip: true
            spacing: Style.spacing.sm

            delegate: Item {
              required property var modelData
              width: ListView.view.width
              // Content-sized: an errored row grows a third caption line.
              height: trCol.implicitHeight + Style.spacing.xs * 2

              Column {
                id: trCol
                anchors.top: parent.top
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.margins: Style.spacing.xs
                spacing: Style.spacing.xs

                Item {
                  width: parent.width
                  height: Style.font.body + Style.spacing.xs

                  Text {
                    anchors.left: parent.left
                    anchors.right: trStatus.left
                    anchors.rightMargin: Style.spacing.md
                    text: (modelData.dir === "in" ? "↓ " : "↑ ") + modelData.file
                    color: root.foreground
                    font.family: root.fontFamily
                    font.pixelSize: Style.font.body
                    elide: Text.ElideMiddle
                  }

                  Text {
                    id: trStatus
                    anchors.right: parent.right
                    text: {
                      switch (modelData.kind) {
                        case "filedone": return "done · " + root.humanBytes(modelData.total)
                        case "error": return "failed"
                        case "cancel": return "cancelled"
                        default: return root.humanBytes(modelData.received)
                                        + " / " + root.humanBytes(modelData.total)
                      }
                    }
                    color: modelData.kind === "error" ? root.urgent
                         : modelData.kind === "filedone" ? root.good
                         : Qt.darker(root.foreground, 1.3)
                    font.family: root.fontFamily
                    font.pixelSize: Style.font.caption
                  }
                }

                Rectangle {
                  width: parent.width
                  height: Style.space(4)
                  radius: height / 2
                  color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.15)

                  Rectangle {
                    width: parent.width * (modelData.total > 0
                             ? Math.min(1, modelData.received / modelData.total)
                             : (modelData.kind === "filedone" ? 1 : 0))
                    height: parent.height
                    radius: height / 2
                    color: modelData.kind === "error" ? root.urgent
                         : modelData.kind === "cancel" ? Qt.darker(root.foreground, 1.5)
                         : root.accent
                  }
                }

                Text {
                  visible: modelData.error !== ""
                  width: parent.width
                  text: modelData.error
                  color: root.urgent
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                  elide: Text.ElideRight
                }
              }
            }

            Text {
              visible: root.transfers.length === 0
              anchors.centerIn: parent
              text: "No transfers."
              color: Qt.darker(root.foreground, 1.4)
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
            }
          }

          // ---- Settings ----
          Column {
            visible: root.tab === 3
            anchors.fill: parent
            spacing: Style.spacing.lg

            PanelSectionHeader { text: "IDENTITY" }

            Row {
              width: parent.width
              spacing: Style.spacing.md

              Text {
                text: "Alias"
                width: Style.space(110)
                anchors.verticalCenter: parent.verticalCenter
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
              }
              TextField {
                id: aliasField
                width: parent.width - Style.space(110) - Style.spacing.md
                placeholderText: "device name"
                // Not bound: a live binding would wipe an in-progress edit
                // when the value changes underneath (CLI, another client).
                Component.onCompleted: text = root.hasService ? root.svc.alias : ""
                Connections {
                  target: root.svc
                  enabled: root.hasService
                  function onAliasChanged() {
                    if (!aliasField.activeFocus) aliasField.text = root.svc.alias
                  }
                }
                onEditingFinished: {
                  var v = String(text).trim()
                  if (root.hasService && v !== "" && v !== root.svc.alias)
                    root.svc.applySettings({ alias: v })
                }
              }
            }

            Row {
              width: parent.width
              spacing: Style.spacing.md

              Text {
                text: "PIN"
                width: Style.space(110)
                anchors.verticalCenter: parent.verticalCenter
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
              }
              TextField {
                id: pinField
                width: parent.width - Style.space(110) - Style.spacing.md
                password: true
                placeholderText: root.hasService && root.svc.pinSet
                                 ? "PIN set — type to change, clear to remove"
                                 : "no PIN — senders connect freely"
                onEditingFinished: {
                  if (root.hasService) root.svc.applySettings({ pin: String(text) })
                }
              }
            }

            PanelSectionHeader { text: "RECEIVING" }

            Row {
              width: parent.width
              spacing: Style.spacing.md

              Text {
                text: "Folder"
                width: Style.space(110)
                anchors.verticalCenter: parent.verticalCenter
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
              }
              TextField {
                id: dirField
                width: parent.width - Style.space(110) - Style.spacing.md
                Component.onCompleted: text = root.hasService ? root.svc.receiveDir : ""
                Connections {
                  target: root.svc
                  enabled: root.hasService
                  function onReceiveDirChanged() {
                    if (!dirField.activeFocus) dirField.text = root.svc.receiveDir
                  }
                }
                onEditingFinished: {
                  var v = String(text).trim()
                  if (root.hasService && v !== "" && v !== root.svc.receiveDir)
                    root.svc.applySettings({ receiveDir: v })
                }
              }
            }

            Row {
              width: parent.width
              spacing: Style.spacing.md

              Text {
                text: "Auto-accept"
                width: Style.space(110)
                anchors.verticalCenter: parent.verticalCenter
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
              }
              ToggleSwitch {
                checked: root.hasService ? root.svc.autoAccept : false
                onToggled: if (root.hasService) root.svc.applySettings({ autoAccept: !root.svc.autoAccept })
              }
            }

            PanelSectionHeader { text: "ENGINE" }

            Text {
              width: parent.width
              text: {
                if (!root.hasService) return "Service not loaded."
                if (root.svc.engineMissing)
                  return "omasend-engine not found. Install it, then reopen:\ncurl -fsSL <install url> | bash"
                return (root.svc.connected ? "Connected" : "Connecting…")
                       + " · port " + root.svc.port
                       + (root.svc.fingerprint ? "\n" + root.svc.fingerprint.substring(0, 16) + "…" : "")
              }
              color: Qt.darker(root.foreground, 1.25)
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              wrapMode: Text.Wrap
            }
          }
        }
      }

      // ----------------------------------------------------------- footer
      Text {
        id: footer
        anchors.bottom: parent.bottom
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottomMargin: card.contentBottomInset
        anchors.leftMargin: card.contentLeftInset
        anchors.rightMargin: card.contentRightInset
        height: Style.font.caption + Style.spacing.sm
        verticalAlignment: Text.AlignBottom
        text: {
          if (root.modalMode !== "") return "enter send · esc cancel"
          switch (root.tab) {
            case 0: return root.stagedPaths.length > 0
                    ? "↑↓ pick device · enter send staged · x clear · 1-4 tabs · esc close"
                    : "↑↓ move · enter/m message · f files · F folder · + add remote · 1-4 tabs · esc close"
            case 1: return "1-4 tabs · esc close"
            case 2: return "c clear finished · 1-4 tabs · esc close"
            default: return "edits save on enter · 1-4 tabs · esc close"
          }
        }
        color: Qt.darker(root.foreground, 1.6)
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        elide: Text.ElideRight
      }

      // ------------------------------------------------------------ modal
      Item {
        anchors.fill: parent
        z: 10
        visible: root.modalMode !== ""

        Rectangle {
          anchors.fill: parent
          color: root.background
          opacity: 0.85
        }

        MouseArea {
          anchors.fill: parent
          onClicked: root.closeModal()
        }

        Rectangle {
          anchors.centerIn: parent
          width: Math.min(Style.space(420), parent.width - Style.space(40))
          height: modalCol.implicitHeight + Style.spacing.xl * 2
          radius: root.cornerRadius
          color: root.background
          border.color: root.border
          border.width: 1

          MouseArea { anchors.fill: parent; onClicked: {} }

          Column {
            id: modalCol
            anchors.fill: parent
            anchors.margins: Style.spacing.xl
            spacing: Style.spacing.lg

            Text {
              width: parent.width
              text: root.modalTitle()
              color: root.accent
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              font.bold: true
              elide: Text.ElideRight
            }

            TextField {
              id: modalInput
              width: parent.width
              password: root.modalMode === "pin"
              placeholderText: root.modalPlaceholder()
              onAccepted: root.submitModal()
              Keys.onEscapePressed: root.closeModal()
            }
          }
        }
      }
    }
  }
}
