package node

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/tunnel"
	"github.com/agentwireprotocol/awp/wire"
)

// Admission policies (section 10.2).
const (
	// AcceptAny admits any key with the default capabilities. It is what
	// the spec recommends for a coding agent on a sandbox.
	AcceptAny = "any"
	// AcceptAllowlist admits only allow-listed or trusted keys, keys we
	// have dialed ourselves, and keys that present an honored grant.
	AcceptAllowlist = "allowlist"
)

// Capabilities with a meaning in this implementation (section 13.1).
const (
	CapExec      = "exec"
	CapFSRead    = "fs:read"
	CapFSWrite   = "fs:write"
	CapIntroduce = "introduce"
	CapAdmin     = "admin"
)

// Policy is the local trust configuration.
type Policy struct {
	Accept string   `json:"accept,omitempty"` // AcceptAny or AcceptAllowlist
	Allow  []string `json:"allow,omitempty"`  // keys admitted under the allowlist
	Trust  []string `json:"trust,omitempty"`  // issuers whose grants we honor as our own

	// Serve lists capabilities this daemon fulfils by itself when a peer
	// with the matching grant asks (exec, fs:read, fs:write). Requests for
	// capabilities not listed here are delivered to the agent instead,
	// marked allowed or denied.
	Serve []string `json:"serve,omitempty"`
	// Root confines fs:read and fs:write and is the default working
	// directory for exec. Empty means the daemon's working directory.
	Root string `json:"root,omitempty"`
}

// requestMimes maps the mime types of capability requests (data parts) to
// the capability they need. The shapes are defined in PROFILE.md.
var requestMimes = map[string]string{
	"application/vnd.awp.exec+json":     CapExec,
	"application/vnd.awp.fs-read+json":  CapFSRead,
	"application/vnd.awp.fs-write+json": CapFSWrite,
	"application/vnd.awp.admin+json":    CapAdmin,
}

// Request is a capability request found in a received msg.
type Request struct {
	Part    int    `json:"part"`
	Mime    string `json:"mime"`
	Cap     string `json:"cap"`
	Allowed bool   `json:"allowed"`
	Served  bool   `json:"served,omitempty"`
}

func (n *Node) trusted(key string) bool {
	return slices.Contains(n.cfg.Policy.Trust, key)
}

// honored is a grant this node honors, with the capabilities it confers.
type honored struct {
	g    *store.GrantRow
	caps []string
}

// honoredGrants applies section 13.2: a grant counts if we issued it, if a
// trusted key issued it, or if its issuer holds an introduce grant from us
// or a trusted key (one level of delegation). A delegated grant is
// attenuated: it confers at most what the introducer itself was granted,
// minus introduce, so delegation cannot chain.
func (n *Node) honoredGrants(peer string, now time.Time) []honored {
	var out []honored
	// A grant bound to another audience (the aud extension) is not ours to
	// honor, even if we issued it: that is an introduction for someone else.
	forUs := func(g *store.GrantRow) bool { return g.Audience == "" || g.Audience == n.key }
	issued, _ := n.st.Grants(store.GrantQuery{Role: store.RoleIssued, Sub: peer, ValidAt: now})
	for _, g := range issued {
		if forUs(g) {
			out = append(out, honored{g, g.Caps})
		}
	}
	presented, _ := n.st.Grants(store.GrantQuery{Role: store.RolePresented, Sub: peer, ValidAt: now})
	for _, g := range presented {
		if !forUs(g) {
			continue
		}
		if g.Iss == n.key || n.trusted(g.Iss) {
			out = append(out, honored{g, g.Caps})
			continue
		}
		var allowed []string
		for _, parent := range n.introducerGrants(g.Iss, now) {
			for _, c := range parent.Caps {
				if c != CapIntroduce && !slices.Contains(allowed, c) {
					allowed = append(allowed, c)
				}
			}
		}
		if n.hasIntroducer(g.Iss, now) {
			var caps []string
			for _, c := range g.Caps {
				if slices.Contains(allowed, c) {
					caps = append(caps, c)
				}
			}
			out = append(out, honored{g, caps})
		}
	}
	return out
}

// introducerGrants returns grants giving key the introduce capability,
// issued by us or by a trusted key.
func (n *Node) introducerGrants(key string, now time.Time) []*store.GrantRow {
	var out []*store.GrantRow
	for _, role := range []string{store.RoleIssued, store.RolePresented} {
		gs, _ := n.st.Grants(store.GrantQuery{Role: role, Sub: key, ValidAt: now})
		for _, g := range gs {
			if (g.Audience == "" || g.Audience == n.key) && slices.Contains(g.Caps, CapIntroduce) && (g.Iss == n.key || n.trusted(g.Iss)) {
				out = append(out, g)
			}
		}
	}
	return out
}

func (n *Node) hasIntroducer(key string, now time.Time) bool {
	return len(n.introducerGrants(key, now)) > 0
}

// Caps returns the capabilities a peer holds on this node beyond the
// defaults.
func (n *Node) Caps(peer string) []string {
	var caps []string
	for _, h := range n.honoredGrants(peer, time.Now()) {
		for _, c := range h.caps {
			if !slices.Contains(caps, c) {
				caps = append(caps, c)
			}
		}
	}
	slices.Sort(caps)
	return caps
}

// admit applies the admission policy after auth.
func (n *Node) admit(peer string, outbound bool) error {
	pol := n.cfg.Policy
	if outbound || pol.Accept != AcceptAllowlist {
		return nil
	}
	if slices.Contains(pol.Allow, peer) || n.trusted(peer) {
		return nil
	}
	if addrs, _ := store.Addrs(n.st.DB(), peer); slices.ContainsFunc(addrs, func(a store.Addr) bool { return a.Source == "connect" }) {
		return nil // we dialed this peer ourselves before
	}
	if len(n.honoredGrants(peer, time.Now())) > 0 {
		return nil
	}
	return errors.New("key is not on this peer's allow list and presented no grant it honors")
}

// acceptPresentedGrant verifies a grant a peer showed about itself (in auth
// or a grant message) and stores it for honoredGrants to consider.
func (n *Node) acceptPresentedGrant(peer string, raw json.RawMessage, via string) {
	now := time.Now()
	g, err := wire.ParseGrant(raw, now)
	if err != nil {
		n.logf("grant presented by %s in %s rejected: %v", wire.ShortKey(peer), via, err)
		return
	}
	if g.Sub != peer {
		n.logf("grant presented by %s in %s is for another key; ignored", wire.ShortKey(peer), via)
		return
	}
	if err := n.st.Tx(func(q store.Q) error { return store.PutGrant(q, grantRow(g, store.RolePresented, "", now)) }); err != nil {
		n.logf("store presented grant: %v", err)
	}
}

// grantsToPresent picks the grants we hold that concern the peer we are
// talking to: ones it issued, and introductions meant for it. Other grants
// stay private.
func (n *Node) grantsToPresent(s *tunnel.Stream) []json.RawMessage {
	held, err := n.st.Grants(store.GrantQuery{Role: store.RoleHeld, Sub: n.key, ValidAt: time.Now()})
	if err != nil {
		return nil
	}
	is := func(key string) bool {
		pub, err := wire.ParseKey(key)
		return err == nil && s.Is(pub)
	}
	var out []json.RawMessage
	for _, g := range held {
		if is(g.Iss) || (g.Audience != "" && is(g.Audience)) {
			out = append(out, json.RawMessage(g.Raw))
		}
	}
	return out
}

// inspectRequests finds capability requests in a msg and checks each
// against the sender's grants.
func (n *Node) inspectRequests(peer string, m *wire.Msg) []Request {
	var reqs []Request
	var caps []string
	for i, p := range m.Parts {
		if p.K != wire.PartData {
			continue
		}
		mt := strings.ToLower(strings.TrimSpace(p.Mime))
		cap, ok := requestMimes[mt]
		if !ok {
			continue
		}
		if caps == nil {
			caps = append(n.Caps(peer), "") // non-nil even when empty
		}
		reqs = append(reqs, Request{Part: i, Mime: mt, Cap: cap, Allowed: slices.Contains(caps, cap)})
	}
	return reqs
}
