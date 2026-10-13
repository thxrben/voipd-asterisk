package directory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"

	"voipd/internal/numbers"
)

// LDAP protocol operations (RFC 4511, application tags).
const (
	opBindRequest      = 0
	opBindResponse     = 1
	opUnbindRequest    = 2
	opSearchRequest    = 3
	opSearchEntry      = 4
	opSearchDone       = 5
	opModifyRequest    = 6
	opAddRequest       = 8
	opDelRequest       = 10
	opModDNRequest     = 12
	opCompareRequest   = 14
	opAbandonRequest   = 16
	opExtendedRequest  = 23
	opExtendedResponse = 24
)

// LDAP result codes.
const (
	resultSuccess            = 0
	resultProtocolError      = 2
	resultSizeLimitExceeded  = 4
	resultNoSuchObject       = 32
	resultInvalidCredentials = 49
	resultInsufficientAccess = 50
	resultUnwillingToPerform = 53
)

const (
	defaultMaxResults = 500
	idleTimeout       = 5 * time.Minute
)

func init() {
	// Requests from phones are tiny; refuse absurd lengths early.
	ber.MaxPacketLengthBytes = 1 << 20
}

// LDAP is a read-only LDAP v3 server for phone books: simple bind with a
// directory account, then search. Writes are refused.
type LDAP struct {
	dir *Directory
	acc *Accounts
	m   matcher
	log *slog.Logger
	// MaxResults caps every search (phones rarely ask for a limit).
	MaxResults int

	listening atomic.Bool
	mu        sync.Mutex
	addr      string
	lastErr   string
}

func NewLDAP(dir *Directory, acc *Accounts, z *numbers.Normalizer, log *slog.Logger) *LDAP {
	return &LDAP{dir: dir, acc: acc, m: matcher{z: z}, log: log, MaxResults: defaultMaxResults}
}

// Status reports whether the server listens (for the UI).
func (l *LDAP) Status() (listening bool, addr, lastErr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listening.Load(), l.addr, l.lastErr
}

// SetError records why the server does not listen.
func (l *LDAP) SetError(addr string, err error) {
	l.mu.Lock()
	l.addr, l.lastErr = addr, err.Error()
	l.mu.Unlock()
}

// Serve accepts connections on ln until ctx ends.
func (l *LDAP) Serve(ctx context.Context, ln net.Listener) error {
	l.mu.Lock()
	l.addr, l.lastErr = ln.Addr().String(), ""
	l.mu.Unlock()
	l.listening.Store(true)
	defer l.listening.Store(false)
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.serveConn(ctx, c)
		}()
	}
}

type session struct {
	ip    string
	bound bool // logged in with a directory account
	user  string
}

func (l *LDAP) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	s := &session{ip: hostOf(c.RemoteAddr())}
	for {
		c.SetReadDeadline(time.Now().Add(idleTimeout))
		p, err := ber.ReadPacket(c)
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil && !isClosed(err) {
				l.log.Debug("ldap: read", "client", s.ip, "err", err)
			}
			return
		}
		if len(p.Children) < 2 {
			return
		}
		id, ok := p.Children[0].Value.(int64)
		if !ok {
			return
		}
		op := p.Children[1]
		c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		var out []*ber.Packet
		switch op.Tag {
		case opBindRequest:
			out = []*ber.Packet{l.bind(ctx, s, op)}
		case opUnbindRequest:
			return
		case opSearchRequest:
			out = l.search(ctx, s, op)
		case opAbandonRequest:
			continue
		case opExtendedRequest: // e.g. StartTLS: not offered
			out = []*ber.Packet{result(opExtendedResponse, resultProtocolError, "extended operations are not supported")}
		case opModifyRequest, opAddRequest, opDelRequest, opModDNRequest, opCompareRequest:
			out = []*ber.Packet{result(int(op.Tag)+1, resultUnwillingToPerform, "the directory is read-only")}
		default:
			return
		}
		for _, o := range out {
			msg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAPMessage")
			msg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "id"))
			msg.AppendChild(o)
			if _, err := c.Write(msg.Bytes()); err != nil {
				return
			}
		}
	}
}

func isClosed(err error) bool {
	return strings.Contains(err.Error(), "use of closed network connection")
}

func hostOf(a net.Addr) string {
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return a.String()
}

func result(op, code int, msg string) *ber.Packet {
	p := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ber.Tag(op), nil, "result")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, "code"))
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, msg, "message"))
	return p
}

// bindUser accepts "user", "uid=user,…" and "cn=user,…".
func bindUser(name string) string {
	first := strings.SplitN(name, ",", 2)[0]
	if k, v, ok := strings.Cut(first, "="); ok {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "uid", "cn":
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(name)
}

func (l *LDAP) bind(ctx context.Context, s *session, op *ber.Packet) *ber.Packet {
	s.bound, s.user = false, ""
	if len(op.Children) < 3 {
		return result(opBindResponse, resultProtocolError, "malformed bind")
	}
	name := str(op.Children[1])
	auth := op.Children[2]
	if auth.ClassType != ber.ClassContext || auth.Tag != 0 {
		return result(opBindResponse, resultUnwillingToPerform, "only simple bind is supported")
	}
	password := str(auth)
	if name == "" && password == "" {
		return result(opBindResponse, resultSuccess, "") // anonymous: may only read the root DSE
	}
	user := bindUser(name)
	if _, err := l.acc.Verify(ctx, user, password, s.ip); err != nil {
		if errors.Is(err, ErrLocked) {
			l.log.Warn("ldap: login blocked after failures", "client", s.ip)
			return result(opBindResponse, resultUnwillingToPerform, err.Error())
		}
		l.log.Info("ldap: login failed", "client", s.ip, "user", user)
		return result(opBindResponse, resultInvalidCredentials, "invalid credentials")
	}
	s.bound, s.user = true, user
	return result(opBindResponse, resultSuccess, "")
}

// search answers a SearchRequest: entries, then done.
func (l *LDAP) search(ctx context.Context, s *session, op *ber.Packet) []*ber.Packet {
	if len(op.Children) < 8 {
		return []*ber.Packet{result(opSearchDone, resultProtocolError, "malformed search")}
	}
	base := normDN(str(op.Children[0]))
	scope, _ := op.Children[1].Value.(int64)
	sizeLimit, _ := op.Children[3].Value.(int64)
	typesOnly, _ := op.Children[5].Value.(bool)
	filter := op.Children[6]
	var want []string
	for _, a := range op.Children[7].Children {
		want = append(want, str(a))
	}

	if base == "" && scope == 0 { // root DSE, also for anonymous clients
		return []*ber.Packet{l.entry(rootDSE(), want, typesOnly), result(opSearchDone, resultSuccess, "")}
	}
	if !s.bound {
		return []*ber.Packet{result(opSearchDone, resultInsufficientAccess, "log in with a directory account first")}
	}
	nodes, err := l.tree(ctx)
	if err != nil {
		l.log.Error("ldap: directory", "err", err)
		return []*ber.Packet{result(opSearchDone, resultUnwillingToPerform, "directory unavailable")}
	}
	found := false
	for _, n := range nodes {
		if normDN(n.dn) == base {
			found = true
			break
		}
	}
	if !found {
		return []*ber.Packet{result(opSearchDone, resultNoSuchObject, "no such object")}
	}
	limit := l.MaxResults
	if sizeLimit > 0 && int(sizeLimit) < limit {
		limit = int(sizeLimit)
	}
	var out []*ber.Packet
	code := resultSuccess
	for _, n := range nodes {
		dn := normDN(n.dn)
		var inScope bool
		switch scope {
		case 0:
			inScope = dn == base
		case 1:
			inScope = parent(dn) == base
		default:
			inScope = dn == base || strings.HasSuffix(dn, ","+base)
		}
		if !inScope || !l.m.match(filter, n) {
			continue
		}
		if len(out) >= limit {
			code = resultSizeLimitExceeded
			break
		}
		out = append(out, l.entry(n, want, typesOnly))
	}
	l.log.Debug("ldap: search", "client", s.ip, "user", s.user, "base", base, "results", len(out))
	return append(out, result(opSearchDone, code, ""))
}

// tree returns all entries of the directory, containers first.
func (l *LDAP) tree(ctx context.Context) ([]*node, error) {
	entries, err := l.dir.Entries(ctx)
	if err != nil {
		return nil, err
	}
	b, c, e := containerNodes()
	nodes := []*node{b, c, e}
	for _, en := range entries {
		nodes = append(nodes, personNode(en))
	}
	return nodes, nil
}

// entry encodes a SearchResultEntry with the requested attributes.
func (l *LDAP) entry(n *node, want []string, typesOnly bool) *ber.Packet {
	all := len(want) == 0
	var none bool
	sel := map[string]bool{}
	for _, w := range want {
		switch w {
		case "*":
			all = true
		case "1.1":
			none = true
		default:
			sel[strings.ToLower(w)] = true
		}
	}
	p := ber.Encode(ber.ClassApplication, ber.TypeConstructed, opSearchEntry, nil, "entry")
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, n.dn, "dn"))
	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes")
	for _, a := range n.attrs {
		if none && !sel[strings.ToLower(a.name)] || !all && !sel[strings.ToLower(a.name)] {
			continue
		}
		pa := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attribute")
		pa.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, a.name, "type"))
		vals := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "values")
		if !typesOnly {
			for _, v := range a.values {
				vals.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, "value"))
			}
		}
		pa.AppendChild(vals)
		attrs.AppendChild(pa)
	}
	p.AppendChild(attrs)
	return p
}
