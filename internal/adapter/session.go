package adapter

import "fmt"

// CheckServer fences the user-wide registry before any lifecycle mutation.
// Legacy records are adopted by the first reconcile in the existing single
// session. Once stamped, another server must never reinterpret its pane IDs.
func CheckServer(records []WakerRecord, socket string) error {
	if socket == "" {
		return fmt.Errorf("HERDR_SOCKET_PATH is required for lifecycle changes")
	}
	for _, r := range records {
		if r.ServerSocket != "" && canonPath(r.ServerSocket) != canonPath(socket) {
			return fmt.Errorf("single-session registry belongs to server %s (pane %s); skipping server %s", r.ServerSocket, r.PaneID, socket)
		}
	}
	return nil
}
