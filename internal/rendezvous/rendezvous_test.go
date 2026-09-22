package rendezvous

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func env(id, dest, digest, body string) []byte {
	b, _ := json.Marshal(map[string]any{
		"version": 1, "transfer_id": id, "source_host": "mac", "source_handle": "claude",
		"dest_alias": dest, "payload_sha256": digest, "payload_b64": body, "signature": "00",
	})
	return b
}

func do(t *testing.T, srv *httptest.Server, method, path string, body []byte) (int, map[string]json.RawMessage) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestCourierContract(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(store.Handler())
	defer srv.Close()

	// push, replay, conflict
	code, out := do(t, srv, http.MethodPost, "/v1/transfers", env("t1", "heping/codex", "AA", "aGk="))
	if code != 200 || !bytes.Contains(out["receipt"], []byte(`"transport_accepted"`)) {
		t.Fatalf("post: %d %s", code, out["receipt"])
	}
	if code, _ = do(t, srv, http.MethodPost, "/v1/transfers", env("t1", "heping/codex", "aa", "aGk=")); code != 200 {
		t.Fatalf("replay (case-insensitive digest) must be accepted, got %d", code)
	}
	if code, _ = do(t, srv, http.MethodPost, "/v1/transfers", env("t1", "heping/codex", "BB", "aGk=")); code != 409 {
		t.Fatalf("conflicting digest must be 409, got %d", code)
	}
	do(t, srv, http.MethodPost, "/v1/transfers", env("t2", "mac/claude", "CC", "aGk="))

	// poll is per destination alias, bounded, and non-destructive
	for i := 0; i < 2; i++ {
		code, out = do(t, srv, http.MethodGet, "/v1/transfers?dest_alias=heping%2Fcodex&limit=20", nil)
		var envs []json.RawMessage
		_ = json.Unmarshal(out["envelopes"], &envs)
		if code != 200 || len(envs) != 1 || !bytes.Contains(envs[0], []byte(`"t1"`)) {
			t.Fatalf("poll %d: %d %s", i, code, out["envelopes"])
		}
	}
	if code, _ = do(t, srv, http.MethodGet, "/v1/transfers?dest_alias=heping%2Fcodex", nil); code != 400 {
		t.Fatalf("missing limit must be 400, got %d", code)
	}

	// ack retires exactly that transfer; wrong digest or unknown id conflicts
	ack := func(id, digest string) int {
		b, _ := json.Marshal(map[string]any{"receipt": map[string]string{
			"stage": "destination_maildir_committed", "transfer_id": id, "payload_sha256": digest}})
		code, _ := do(t, srv, http.MethodPost, "/v1/transfers/"+id+"/ack", b)
		return code
	}
	if ack("t1", "ZZ") != 409 || ack("nope", "AA") != 409 {
		t.Fatal("ack with wrong digest or unknown id must be 409")
	}
	if ack("t1", "AA") != 200 {
		t.Fatal("ack must succeed")
	}
	code, out = do(t, srv, http.MethodGet, "/v1/transfers?dest_alias=heping%2Fcodex&limit=20", nil)
	if code != 200 || string(out["envelopes"]) != "[]" {
		t.Fatalf("after ack: %d %s", code, out["envelopes"])
	}
	// the other alias is untouched and survives a reopen
	reopened, _ := Open(store.dir)
	envs, _ := reopened.Pending("mac/claude", 5)
	if len(envs) != 1 {
		t.Fatalf("persisted pending for mac/claude: %d", len(envs))
	}
}
