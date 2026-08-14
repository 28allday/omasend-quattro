import QtQuick
import qs.Commons
import qs.Ui

// Bar icon for the Omasend panel. A paper-plane glyph that toggles the panel
// through the same IPC route a keybinding would use; a small badge appears
// when there are unread messages or a transfer is running. State comes from
// the live service instance (bar.shell.serviceFor), so nothing here talks to
// the engine directly.
BarWidget {
  id: root
  moduleName: "nosignal.omasend"

  readonly property string pluginId: "nosignal.omasend"
  readonly property var service: (bar && bar.shell && typeof bar.shell.serviceFor === "function")
                                 ? bar.shell.serviceFor(pluginId) : null

  readonly property int unread: service ? service.unreadMessages : 0
  readonly property int active: service ? service.activeTransfers : 0
  readonly property bool offersPending: service ? service.offers.length > 0 : false
  readonly property bool engineUp: service ? service.connected : false

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  WidgetButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    text: "󰒊"
    tooltipText: {
      if (!root.engineUp) return "Omasend — engine not running"
      var bits = []
      if (root.unread > 0) bits.push(root.unread + " unread")
      if (root.active > 0) bits.push(root.active + " transferring")
      if (root.offersPending) bits.push("incoming offer")
      return bits.length ? "Omasend — " + bits.join(", ") : "Omasend"
    }
    foreground: root.engineUp ? Color.accent : Color.muted
    fixedWidth: root.bar && root.bar.vertical ? -1 : Style.space(27)
    fixedHeight: root.bar && root.bar.vertical ? Style.space(26) : -1
    onPressed: function(b) {
      if (!root.bar) return
      root.bar.run("omarchy-shell shell toggle nosignal.omasend")
    }
  }

  // Activity badge: urgent while an offer waits or messages are unread,
  // accent while a transfer runs.
  Rectangle {
    visible: root.unread > 0 || root.active > 0 || root.offersPending
    width: Style.space(7)
    height: width
    radius: width / 2
    color: (root.unread > 0 || root.offersPending) ? Color.urgent : Color.accent
    anchors.top: parent.top
    anchors.right: parent.right
    anchors.topMargin: Style.space(4)
    anchors.rightMargin: Style.space(3)
  }
}
