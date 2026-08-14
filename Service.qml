import QtQuick
import Quickshell
import Quickshell.Io

// Omasend service plugin for omarchy-shell.
//
// Owns the connection to omasend-engine — the headless LocalSend engine
// (discovery + receiver + sender, a Go daemon) — over a unix socket speaking
// JSON Lines. The engine receives files and messages for as long as the shell
// is running; this service holds the live state (peers, transfers, messages,
// pending offers, settings) that Panel.qml and BarWidget.qml render, raises
// desktop notifications for incoming traffic, and relays actions back.
//
// The engine is spawned here if nothing is already listening on the socket,
// and respawned with a short backoff if it dies. `engineMissing` turns true
// when the binary can't be found so the panel can say how to install it.
Item {
  id: root

  // ---- injected by shell.qml (_syncServices/ensureService) ----
  property var shell: null
  property var manifest: null
  property var pluginRegistry: null

  readonly property string pluginId: "nosignal.omasend"
  readonly property string home: Quickshell.env("HOME")

  // Engine binary + socket. The install script drops the engine in
  // ~/.local/bin; a PATH copy also works via the `sh -c` spawn below.
  readonly property string engineBin: home + "/.local/bin/omasend-engine"
  readonly property string socketPath: {
    var rt = Quickshell.env("XDG_RUNTIME_DIR")
    return rt && rt !== "" ? rt + "/omasend.sock" : "/tmp/omasend.sock"
  }

  // ------------------------------------------------------------------ state
  property bool connected: false
  property bool engineMissing: false
  property bool engineSpawned: false

  // Identity + settings (from ready/status events).
  property string alias: ""
  property string fingerprint: ""
  property int port: 0
  property string receiveDir: ""
  property bool autoAccept: false
  property bool pinSet: false

  // Live collections. Arrays are replaced wholesale (never mutated in place)
  // so QML bindings see the change.
  property var peers: []       // [{alias, ip, model, type, fingerprint, lastSeen}]
  property var messages: []    // [{from, to, text, time, outgoing}]
  property var offers: []      // [{offerId, from, ip, total, files:[{name,size}]}]
  property var transfers: []   // [{id, dir, kind, file, received, total, error}]

  property int unreadMessages: 0
  readonly property int activeTransfers: {
    var n = 0
    for (var i = 0; i < transfers.length; i++) {
      var k = transfers[i].kind
      if (k === "start" || k === "progress") n++
    }
    return n
  }

  // Send results keyed by seq, for the panel to react to (PIN prompt, errors).
  property var lastSendResult: null   // {seq, ok, error, pinRequired, to}
  property int _seq: 0
  property var ipcPending: ({})       // seqs of IPC-initiated sends awaiting a result

  // Register an IPC-initiated send so its sendResult is reported (failures
  // notify — see the sendResult handler) and return the IPC reply string.
  function trackIpcSend(seq) {
    if (seq <= 0) return "engine not connected"
    var pend = {}
    for (var k in root.ipcPending) pend[k] = true
    pend[seq] = true
    root.ipcPending = pend
    return "queued seq " + seq
  }

  // ---------------------------------------------------------------- engine
  // Connection strategy: try the socket first — an engine may already be
  // running (previous shell instance, or started by hand). Only spawn one
  // after a connect attempt fails with nothing listening.
  //
  // Socket quirks (verified against quickshell's socket.cpp): a FAILED
  // connect attempt emits only error() — connectionStateChanged never fires —
  // and the instance wedges permanently (its internal QLocalSocket is only
  // torn down on a clean disconnect). So retries recreate the Socket through
  // a Loader, and the retry path is driven from onError.
  Loader {
    id: sockLoader
    active: false
    sourceComponent: Socket {
      path: root.socketPath
      parser: SplitParser {
        onRead: function(line) { root.handleLine(line) }
      }
      onConnectionStateChanged: {
        root.connected = connected
        if (connected) {
          root.reconnectDelay = 1500   // healthy again: reset the backoff
          root.engineError = ""
        } else {
          root.scheduleReconnect()     // established, then dropped
        }
      }
      onError: function(err) { root.scheduleReconnect() }  // attempt failed
    }
    onLoaded: item.connected = true
  }

  function attemptConnect() {
    sockLoader.active = false   // discard any wedged instance
    sockLoader.active = true
  }

  // Exponential backoff: a persistently failing engine (e.g. port 53317 held
  // by the legacy omarchy-send TUI) must not become a 1.5s fork/exit loop.
  property int reconnectDelay: 1500

  function scheduleReconnect() {
    root.connected = false
    // Nothing listening: (re)start the engine unless it's mid-start or the
    // binary is known missing. A lost race with an external engine is fine —
    // ours exits "already running" and the next connect succeeds.
    if (!engineProc.running && !root.engineMissing) {
      root.engineSpawned = true
      engineProc.running = true
    }
    if (!reconnect.running) {
      reconnect.interval = root.reconnectDelay
      root.reconnectDelay = Math.min(root.reconnectDelay * 2, 30000)
      reconnect.start()
    }
  }

  Timer {
    id: reconnect
    repeat: false
    onTriggered: root.attemptConnect()
  }

  // Last line the engine wrote to stderr before exiting — surfaced in the
  // panel so "engine keeps dying" comes with its reason (port in use, …).
  property string engineError: ""

  Process {
    id: engineProc
    // sh -c so a PATH install works when ~/.local/bin/omasend-engine is
    // absent; exec keeps the engine as the process we track. The socket path
    // is passed explicitly so both sides always agree, whatever the
    // environment's fallback would be.
    command: ["sh", "-c",
      'if [ -x "$1" ]; then exec "$1" -socket "$2"; else exec omasend-engine -socket "$2"; fi',
      "omasend-engine-launch", root.engineBin, root.socketPath]
    stderr: StdioCollector {
      onStreamFinished: {
        var lines = String(text || "").trim().split("\n")
        if (lines.length && lines[lines.length - 1] !== "")
          root.engineError = lines[lines.length - 1]
      }
    }
    onExited: function(code) {
      // 127 = neither binary found. Anything else: crashed or lost a race
      // with an already-running engine; either way just try the socket again.
      if (code === 127) root.engineMissing = true
      if (!reconnect.running) {
        reconnect.interval = root.reconnectDelay
        root.reconnectDelay = Math.min(root.reconnectDelay * 2, 30000)
        reconnect.start()
      }
    }
  }

  Component.onCompleted: attemptConnect()

  // ---------------------------------------------------------------- wire
  function request(obj) {
    var s = sockLoader.item
    if (!s || !s.connected) return false
    s.write(JSON.stringify(obj) + "\n")
    s.flush()
    return true
  }

  function handleLine(line) {
    var t = String(line || "").trim()
    if (t === "") return
    var ev
    try { ev = JSON.parse(t) } catch (e) {
      console.warn("omasend: bad event line:", t.substring(0, 120))
      return
    }
    switch (ev.event) {
      case "ready":
      case "status":
        root.alias = ev.alias || ""
        root.fingerprint = ev.fingerprint || ""
        root.port = ev.port || 0
        root.receiveDir = ev.receiveDir || ""
        root.autoAccept = ev.autoAccept === true
        root.pinSet = ev.pinSet === true
        if (ev.event === "ready") {
          root.engineMissing = false
          root.offers = []          // engine restarted: parked offers are gone
          // In-flight rows belong to the previous engine and can never
          // finish — mark them cancelled so the sweep clears them.
          var now = Date.now()
          root.transfers = root.transfers.map(function(tr) {
            if (tr.kind === "start" || tr.kind === "progress") {
              var c = {}
              for (var k in tr) c[k] = tr[k]
              c.kind = "cancel"
              c.doneAt = now
              return c
            }
            return tr
          })
        }
        break
      case "peers":
        root.peers = ev.peers || []
        break
      case "messages":
        root.messages = ev.messages || []
        break
      case "message":
        root.messages = root.messages.concat([{
          from: ev.from || "", to: ev.to || "", text: ev.text || "",
          time: ev.time || "", outgoing: ev.outgoing === true
        }])
        if (ev.outgoing !== true) {
          root.unreadMessages++
          root.notify("Message from " + (ev.from || "someone"), ev.text || "")
        }
        break
      case "offer":
        root.offers = root.offers.concat([{
          offerId: ev.offerId, from: ev.from || "", ip: ev.ip || "",
          total: ev.total || 0, files: ev.files || []
        }])
        root.notify((ev.from || "Someone") + " wants to send files",
                    root.offerBody(ev.files || []))
        break
      case "offerDone":
        root.offers = root.offers.filter(function(o) { return o.offerId !== ev.offerId })
        break
      case "transfer":
        root.applyTransfer(ev)
        break
      case "sendResult":
        root.lastSendResult = {
          seq: ev.seq, ok: ev.ok === true, error: ev.error || "",
          pinRequired: ev.pinRequired === true, to: ev.to || ""
        }
        // Picker-initiated sends have no panel watching them, so report the
        // outcome as a notification. A PIN-gated peer re-opens the panel with
        // the same files staged — its Enter-send path owns the PIN prompt.
        if (ev.seq === root.pickerSeq && root.pickerSeq > 0) {
          root.pickerSeq = -1
          if (ev.ok === true) {
            root.notify("Sent to " + (ev.to || "peer"), "")
          } else if (ev.pinRequired === true) {
            root.notify("PIN required",
                        "Pick the device and press Enter, then type its PIN")
            root.summonStaged(root.pickerPaths)
          } else {
            root.notify("Send failed", ev.error || "")
          }
          root.pickerPaths = []
        }
        // IPC-initiated sends (omarchy-shell omasend send …) equally have no
        // panel watching; without this their failures vanish silently.
        if (root.ipcPending[ev.seq] === true) {
          var pend = {}
          for (var k in root.ipcPending) if (Number(k) !== ev.seq) pend[k] = true
          root.ipcPending = pend
          if (ev.ok !== true)
            root.notify("Send failed", ev.error || "peer unreachable")
        }
        break
    }
  }

  // Upsert by transfer ID. Terminal rows are stamped so the sweep below can
  // age them out; errors stay until cleared from the panel.
  function isTerminalKind(kind) {
    return kind === "filedone" || kind === "cancel" || kind === "error"
  }

  function applyTransfer(ev) {
    var doneAt = root.isTerminalKind(ev.kind) ? Date.now() : 0
    var next = root.transfers.slice()
    var found = false
    for (var i = 0; i < next.length; i++) {
      if (next[i].id === ev.id) {
        next[i] = {
          id: ev.id, dir: ev.dir, kind: ev.kind, file: ev.file || next[i].file,
          received: ev.received || 0, total: ev.total || next[i].total,
          error: ev.error || "", doneAt: doneAt
        }
        found = true
        break
      }
    }
    if (!found) next.push({
      id: ev.id, dir: ev.dir, kind: ev.kind, file: ev.file || "",
      received: ev.received || 0, total: ev.total || 0, error: ev.error || "",
      doneAt: doneAt
    })
    root.transfers = next
    // Aggregate received-file notifications: a folder of N files must raise
    // one "Received N files" toast, not N separate ones. The panel-open check
    // happens HERE, at arrival — the user watched this land in the open
    // panel, so no toast later even if they close it before the debounce
    // fires (that late pop read as a flash over the closing panel).
    if (ev.dir === "in" && ev.kind === "filedone" && !root.panelOpen()) {
      root.recvBatchCount++
      root.recvBatchLast = ev.file || "a file"
      recvBatch.restart()
    }
  }

  property int recvBatchCount: 0
  property string recvBatchLast: ""

  Timer {
    id: recvBatch
    interval: 1200
    repeat: false
    onTriggered: {
      if (root.recvBatchCount === 1)
        root.notify("Received " + root.recvBatchLast, "Saved to " + root.receiveDir)
      else if (root.recvBatchCount > 1)
        root.notify("Received " + root.recvBatchCount + " files", "Saved to " + root.receiveDir)
      root.recvBatchCount = 0
      root.recvBatchLast = ""
    }
  }

  // Completed and cancelled rows clear themselves shortly after finishing;
  // failed rows are kept so the error stays visible until cleared by hand.
  readonly property int doneLingerMs: 8000

  Timer {
    interval: 2000
    repeat: true
    running: root.transfers.length > 0
    onTriggered: {
      var now = Date.now()
      var keep = root.transfers.filter(function(tr) {
        if (tr.kind === "filedone" || tr.kind === "cancel")
          return tr.doneAt > 0 && (now - tr.doneAt) < root.doneLingerMs
        return true
      })
      if (keep.length !== root.transfers.length) root.transfers = keep
    }
  }

  function offerBody(files) {
    if (files.length === 1) return files[0].name
    return files.length + " files"
  }

  // True while our panel is on screen (host-tracked open flag).
  function panelOpen() {
    return root.shell && root.shell.openPanelIds
        && root.shell.openPanelIds[root.pluginId] === true
  }

  function notify(summary, body) {
    // The panel anchors top-right — the same corner notifications pop in —
    // so a toast would sit exactly over the offer strip's Accept/Decline.
    // While the panel is open it IS the UI; stay quiet.
    if (root.panelOpen()) return
    Quickshell.execDetached(["notify-send", "-a", "Omasend", summary, body])
  }

  // ---------------------------------------------------------------- actions
  // All return the request's seq (or -1 when not connected) so callers can
  // match the sendResult event.
  function sendMessage(to, ip, text, pin) {
    root._seq++
    var ok = root.request({ req: "send", seq: root._seq, to: to || "", ip: ip || "",
                            message: text, pin: pin || "" })
    return ok ? root._seq : -1
  }

  function sendFiles(to, ip, paths, pin) {
    root._seq++
    var ok = root.request({ req: "send", seq: root._seq, to: to || "", ip: ip || "",
                            paths: paths, pin: pin || "" })
    return ok ? root._seq : -1
  }

  function acceptOffer(offerId, accept) {
    root.request({ req: "accept", offerId: offerId, accept: accept === true })
  }

  function applySettings(obj) {
    var req = { req: "set" }
    if (obj.alias !== undefined) req.alias = String(obj.alias)
    if (obj.pin !== undefined) req.setPin = String(obj.pin)
    if (obj.receiveDir !== undefined) req.receiveDir = String(obj.receiveDir)
    if (obj.autoAccept !== undefined) req.autoAccept = obj.autoAccept === true
    root.request(req)
  }

  function addPeer(host) {
    root.request({ req: "addPeer", host: String(host || "") })
  }

  // ------------------------------------------------------------ file picker
  // "Send a file" from the panel opens a GTK file chooser (zenity) and sends
  // the selection straight to the chosen peer — the panel closes first so the
  // dialog isn't fighting a keyboard-exclusive overlay. Returns false when
  // zenity isn't available, letting the panel fall back to its path modal.
  property bool hasZenity: false
  property var pickPeer: null
  property int pickerSeq: -1
  property var pickerPaths: []

  function summonStaged(paths) {
    if (!paths || paths.length === 0) return
    var payload = JSON.stringify({ paths: paths })
    if (root.shell && typeof root.shell.summon === "function")
      root.shell.summon(root.pluginId, payload)
    else
      Quickshell.execDetached(["omarchy-shell", "shell", "summon", root.pluginId, payload])
  }

  Process {
    id: zenityCheck
    running: true
    command: ["sh", "-c", "command -v zenity"]
    onExited: function(code) { root.hasZenity = (code === 0) }
  }

  Process {
    id: picker
    stdout: StdioCollector {
      onStreamFinished: root.pickerDone(text)
    }
  }

  // wantDir: false = multi-select files, true = pick folders (sent whole,
  // expanded on the wire by the engine).
  function pickAndSend(peer, wantDir) {
    if (!root.hasZenity) {
      // Re-probe so a zenity installed after shell start is picked up on
      // the next attempt without a shell restart.
      zenityCheck.running = true
      return false
    }
    if (picker.running || !peer) return false
    root.pickPeer = peer
    var cmd = ["zenity", "--file-selection", "--multiple", "--separator=\n"]
    if (wantDir === true) cmd.push("--directory")
    cmd.push("--title", (wantDir === true ? "Send folder to " : "Send to ")
                        + peer.alias + " via Omasend")
    picker.command = cmd
    picker.running = true
    return true
  }

  function pickerDone(out) {
    var peer = root.pickPeer
    root.pickPeer = null
    var paths = String(out || "").split("\n").map(function(p) {
      return p.trim()
    }).filter(function(p) { return p !== "" })
    if (!peer || paths.length === 0) return   // dialog cancelled
    root.pickerPaths = paths
    root.pickerSeq = root.sendFiles("", peer.ip, paths, "")
    if (root.pickerSeq < 0)
      root.notify("Omasend", "Engine not connected — could not send")
    else
      root.notify("Sending " + paths.length + " item(s) to " + peer.alias, "")
  }

  function clearUnread() { root.unreadMessages = 0 }

  // Drop finished/errored rows, keeping live ones.
  function pruneTransfers() {
    root.transfers = root.transfers.filter(function(tr) {
      return tr.kind === "start" || tr.kind === "progress"
    })
  }

  // Drop a single row (the panel's per-row dismiss on failed transfers).
  function dismissTransfer(id) {
    root.transfers = root.transfers.filter(function(tr) { return tr.id !== id })
  }

  // ---------------------------------------------------------------- IPC
  IpcHandler {
    target: "omasend"

    function ping(): string { return root.connected ? "ok" : "engine not connected" }

    function status(): string {
      return JSON.stringify({
        connected: root.connected,
        engineMissing: root.engineMissing,
        alias: root.alias,
        peers: root.peers.length,
        unreadMessages: root.unreadMessages,
        activeTransfers: root.activeTransfers,
        listedTransfers: root.transfers.length
      })
    }

    // omarchy-shell omasend send '<alias>' '<text>'
    function send(to: string, text: string): string {
      var seq = root.sendMessage(to, "", text, "")
      return root.trackIpcSend(seq)
    }

    // omarchy-shell omasend sendFile '<alias>' '<absolute path>'
    // Files or folders (folders expand on the wire). One path per call.
    function sendFile(to: string, path: string): string {
      var p = String(path || "").trim()
      if (p === "") return "usage: sendFile <alias> <absolute-path>"
      var seq = root.sendFiles(to, "", [p], "")
      return root.trackIpcSend(seq)
    }
  }
}
