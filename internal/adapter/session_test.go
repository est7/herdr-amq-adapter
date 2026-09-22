package adapter

import "testing"

func TestServerFence(t *testing.T) {
	recs := []WakerRecord{{PaneID: "w1:p1", Handle: "claude", ServerSocket: "/server-a.sock"}}
	if err := CheckServer(recs, "/server-b.sock"); err == nil {
		t.Fatal("foreign server may retire colliding pane")
	}
	if err := CheckServer(recs, "/server-a.sock"); err != nil {
		t.Fatal(err)
	}
	if err := CheckServer(recs, ""); err == nil {
		t.Fatal("missing authority accepted")
	}
	if err := CheckServer([]WakerRecord{{PaneID: "w1:p1"}}, "/server-a.sock"); err != nil {
		t.Fatal(err)
	}
}
