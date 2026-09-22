// Package rendezvous is the store-and-forward blob service amq-bridge's
// HTTPS courier talks to. It is deliberately dumb: it never reads AMQ
// handles, Maildir state, or envelope payloads. It keys transfers by
// (transfer_id) and dest_alias, dedupes by payload digest, hands envelopes
// out on poll, and drops them on a destination_maildir_committed ack.
//
// Contract (from amq-bridge cmd/amq-bridge/courier.go, v0.80.1):
//
//	POST /v1/transfers            body: envelope JSON
//	                              200 {"receipt":{"stage":"transport_accepted","transfer_id","payload_sha256"}}
//	                              409 when the same transfer_id carries a different digest
//	GET  /v1/transfers?dest_alias=<host/agent>&limit=<n>
//	                              200 {"envelopes":[<envelope JSON>...]}
//	POST /v1/transfers/<id>/ack   body: {"receipt":{"stage":"destination_maildir_committed","transfer_id","payload_sha256"}}
//	                              200 {"receipt": same}; 409 on unknown id or digest mismatch
//
// The courier only accepts http on loopback, so the service binds loopback
// and peers reach it through an SSH tunnel.
package rendezvous

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/est7/herdr-amq-adapter/internal/durable"
)

const transfersPath = "/v1/transfers"

// envelopeHead is the subset of a bridge envelope the service keys on.
type envelopeHead struct {
	TransferID    string `json:"transfer_id"`
	DestAlias     string `json:"dest_alias"`
	PayloadSHA256 string `json:"payload_sha256"`
}

type wireReceipt struct {
	Stage         string `json:"stage"`
	TransferID    string `json:"transfer_id"`
	PayloadSHA256 string `json:"payload_sha256"`
}

// Store keeps every pending envelope as one file under dir, named by
// transfer id, so a restart loses nothing. Acked transfers are removed.
type Store struct {
	mu  sync.Mutex
	dir string
}

func Open(dir string) (*Store, error) {
	if err := durable.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("rendezvous dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, id)+".envelope")
}

type stored struct {
	Head     envelopeHead
	Raw      json.RawMessage
	Received time.Time
}

func (s *Store) load(id string) (stored, bool, error) {
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return stored{}, false, nil
	}
	if err != nil {
		return stored{}, false, err
	}
	var st stored
	if err := json.Unmarshal(b, &st); err != nil {
		return stored{}, false, err
	}
	return st, true, nil
}

func (s *Store) save(st stored) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return durable.WriteFile(s.path(st.Head.TransferID), b, 0o600)
}

// Accept stores the envelope; the same transfer with the same digest is an
// idempotent replay, a different digest is a conflict.
func (s *Store) Accept(raw json.RawMessage) (wireReceipt, error) {
	var head envelopeHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return wireReceipt{}, errBad("decode envelope: " + err.Error())
	}
	if head.TransferID == "" || head.DestAlias == "" || head.PayloadSHA256 == "" {
		return wireReceipt{}, errBad("envelope needs transfer_id, dest_alias, payload_sha256")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok, err := s.load(head.TransferID)
	if err != nil {
		return wireReceipt{}, err
	}
	if ok {
		if !strings.EqualFold(prev.Head.PayloadSHA256, head.PayloadSHA256) {
			return wireReceipt{}, errConflict("transfer digest conflict")
		}
		// A previous directory sync may have failed after rename. Re-publish
		// before acknowledging a retry rather than trusting visibility alone.
		if err := s.save(prev); err != nil {
			return wireReceipt{}, err
		}
	} else if err := s.save(stored{Head: head, Raw: raw, Received: time.Now()}); err != nil {
		return wireReceipt{}, err
	}
	return wireReceipt{Stage: "transport_accepted", TransferID: head.TransferID, PayloadSHA256: head.PayloadSHA256}, nil
}

// Pending returns up to limit envelopes for one destination alias, oldest
// first. Envelopes stay until acked; a poller that crashes mid-apply sees
// them again, and amq-bridge's transfer ledger makes that a replay.
func (s *Store) Pending(destAlias string, limit int) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var all []stored
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".envelope") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var st stored
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("corrupt %s: %w", e.Name(), err)
		}
		if st.Head.DestAlias == destAlias {
			all = append(all, st)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Received.Before(all[j].Received) })
	out := make([]json.RawMessage, 0, limit)
	for i := 0; i < len(all) && i < limit; i++ {
		out = append(out, all[i].Raw)
	}
	return out, nil
}

// Ack retires a transfer once the destination committed it to its Maildir.
func (s *Store) Ack(r wireReceipt) (wireReceipt, error) {
	if r.Stage != "destination_maildir_committed" {
		return wireReceipt{}, errBad("ack stage must be destination_maildir_committed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok, err := s.load(r.TransferID)
	if err != nil {
		return wireReceipt{}, err
	}
	if !ok || !strings.EqualFold(st.Head.PayloadSHA256, r.PayloadSHA256) {
		return wireReceipt{}, errConflict("ack conflict")
	}
	if err := os.Remove(s.path(r.TransferID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return wireReceipt{}, err
	}
	return r, nil
}

// Handler serves the courier contract.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(transfersPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var raw json.RawMessage
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&raw); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			receipt, err := s.Accept(raw)
			if err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, map[string]any{"receipt": receipt})
		case http.MethodGet:
			limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil || limit < 1 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			dest := r.URL.Query().Get("dest_alias")
			if dest == "" {
				http.Error(w, "dest_alias is required", http.StatusBadRequest)
				return
			}
			envs, err := s.Pending(dest, limit)
			if err != nil {
				fail(w, err)
				return
			}
			if envs == nil {
				envs = []json.RawMessage{}
			}
			writeJSON(w, map[string]any{"envelopes": envs})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(transfersPath+"/", func(w http.ResponseWriter, r *http.Request) {
		id, isAck := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, transfersPath+"/"), "/ack")
		if !isAck || r.Method != http.MethodPost || id == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req struct {
			Receipt wireReceipt `json:"receipt"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Receipt.TransferID != id {
			http.Error(w, "receipt transfer_id does not match path", http.StatusBadRequest)
			return
		}
		receipt, err := s.Ack(req.Receipt)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"receipt": receipt})
	})
	return mux
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }
func errBad(m string) error       { return httpError{http.StatusBadRequest, m} }
func errConflict(m string) error  { return httpError{http.StatusConflict, m} }

func fail(w http.ResponseWriter, err error) {
	var he httpError
	if errors.As(err, &he) {
		http.Error(w, he.msg, he.code)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
