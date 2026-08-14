package server

import "omasend/internal/protocol"

// AcceptDecision is the user's response to an incoming upload request.
type AcceptDecision struct {
	Accept bool
}

// AcceptRequest is raised when a peer asks to upload. The prepare-upload
// handler blocks on Reply until the TUI (or auto-accept) answers.
type AcceptRequest struct {
	From      protocol.DeviceInfo
	IP        string
	Files     map[string]protocol.FileMetadata
	TotalSize int64
	Reply     chan AcceptDecision
}
