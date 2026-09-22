// Package bridge federates the per-machine AMQ roots that this plugin owns
// with amq-bridge, the upstream cross-host courier. Every host keeps its own
// root and its own agents; a remote agent is visible locally as an alias
// mailbox named <host>-<agent>. Mail dropped into an alias mailbox is
// re-addressed and handed to `amq-bridge enqueue`; the courier moves signed
// envelopes through a rendezvous that one host serves on loopback and the
// others reach over an SSH tunnel; the receiving courier applies them into
// the real agent's inbox, where the ordinary waker rings the doorbell.
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// AliasHandle is the local mailbox name under which a remote agent is
// reachable: <host>-<agent>. It is also the name a peer sees this host's
// agents under, so a message crossing hosts carries alias names on both
// ends and never claims a peer's local handle.
func AliasHandle(host, agent string) string { return host + "-" + agent }

// DestAlias is amq-bridge's receiver-owned address for a remote agent.
func DestAlias(host, agent string) string { return host + "/" + agent }

var handleRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]*$`)

var frontMatter = regexp.MustCompile(`(?s)\A---json\n(.*?)\n---\n(.*)\z`)

// Readdress rewrites an AMQ message file for the wire: from becomes the
// alias the receiving host knows the sender by, to becomes the receiving
// agent's real handle. Every other header and the body are kept verbatim,
// so id, thread, subject, refs survive the hop. amq-bridge enqueue requires
// from to equal the spool's source handle, which is why from is rewritten
// at the source rather than by the receiver.
func Readdress(msg []byte, from, to string) ([]byte, error) {
	m := frontMatter.FindSubmatch(msg)
	if m == nil {
		return nil, fmt.Errorf("message is not an AMQ ---json front-matter file")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(m[1], &header); err != nil {
		return nil, fmt.Errorf("decode message header: %w", err)
	}
	if _, ok := header["id"]; !ok {
		return nil, fmt.Errorf("message header has no id")
	}
	header["from"], _ = json.Marshal(from)
	header["to"], _ = json.Marshal([]string{to})
	hb, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("---json\n")
	out.Write(hb)
	out.WriteString("\n---\n")
	out.Write(m[2])
	return out.Bytes(), nil
}
