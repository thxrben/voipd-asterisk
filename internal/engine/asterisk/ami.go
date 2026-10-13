package asterisk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Message is one AMI packet. Keys are case-sensitive as sent by Asterisk;
// repeated keys (e.g. "Output") are joined with "\n".
type Message map[string]string

func (m Message) Get(k string) string { return m[k] }

// amiConn is one AMI session. It does not reconnect; Engine.Run owns that.
type amiConn struct {
	conn   net.Conn
	w      *bufio.Writer
	wmu    sync.Mutex
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[string]*amiCall
	closed  bool
	err     error

	events chan Message
	done   chan struct{}
}

type amiCall struct {
	resp   Message
	list   []Message
	isList bool
	done   chan struct{}
}

var errAMIClosed = errors.New("ami: connection closed")

func dialAMI(ctx context.Context, addr, user, secret string) (*amiConn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	banner, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ami banner: %w", err)
	}
	if !strings.HasPrefix(banner, "Asterisk Call Manager") {
		conn.Close()
		return nil, fmt.Errorf("ami: unexpected banner %q", strings.TrimSpace(banner))
	}
	conn.SetReadDeadline(time.Time{})

	c := &amiConn{
		conn:    conn,
		w:       bufio.NewWriter(conn),
		pending: map[string]*amiCall{},
		events:  make(chan Message, 256),
		done:    make(chan struct{}),
	}
	go c.readLoop(r)

	resp, _, err := c.Action(ctx, "Login", "Username", user, "Secret", secret, "Events", "on")
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.Get("Response") != "Success" {
		c.Close()
		return nil, fmt.Errorf("ami login: %s", resp.Get("Message"))
	}
	return c, nil
}

// Events returns unsolicited events. It is closed when the connection dies.
func (c *amiConn) Events() <-chan Message { return c.events }

// Done is closed when the connection dies; Err then reports why.
func (c *amiConn) Done() <-chan struct{} { return c.done }

func (c *amiConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *amiConn) Close() error {
	c.shutdown(errAMIClosed)
	return nil
}

// Action sends an action and waits for its response. For actions that
// answer with an event list (EventList: start), the list events are
// returned as the second value.
func (c *amiConn) Action(ctx context.Context, action string, kv ...string) (Message, []Message, error) {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	call := &amiCall{done: make(chan struct{})}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, c.err
	}
	c.pending[id] = call
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	var b strings.Builder
	fmt.Fprintf(&b, "Action: %s\r\nActionID: %s\r\n", action, id)
	for i := 0; i+1 < len(kv); i += 2 {
		if strings.ContainsAny(kv[i]+kv[i+1], "\r\n") {
			return nil, nil, fmt.Errorf("ami: field %s contains newline", kv[i])
		}
		fmt.Fprintf(&b, "%s: %s\r\n", kv[i], kv[i+1])
	}
	b.WriteString("\r\n")

	c.wmu.Lock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.w.WriteString(b.String())
	if err == nil {
		err = c.w.Flush()
	}
	c.wmu.Unlock()
	if err != nil {
		c.shutdown(err)
		return nil, nil, err
	}

	select {
	case <-call.done:
		if r := call.resp.Get("Response"); r == "Error" {
			return call.resp, call.list, fmt.Errorf("ami %s: %s", action, call.resp.Get("Message"))
		}
		return call.resp, call.list, nil
	case <-c.done:
		return nil, nil, c.Err()
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (c *amiConn) readLoop(r *bufio.Reader) {
	for {
		m, err := readMessage(r)
		if err != nil {
			c.shutdown(err)
			return
		}
		c.dispatch(m)
	}
}

func (c *amiConn) dispatch(m Message) {
	id := m.Get("ActionID")
	c.mu.Lock()
	call := c.pending[id]
	c.mu.Unlock()

	if call == nil {
		if m.Get("Event") != "" {
			select {
			case c.events <- m:
			default:
				// Never block the reader: a stuck consumer must not stall
				// action responses. Dropped events are recovered by polling.
			}
		}
		return
	}

	switch {
	case m.Get("Response") != "":
		call.resp = m
		if strings.EqualFold(m.Get("EventList"), "start") {
			call.isList = true
			return
		}
		close(call.done)
	case call.isList && strings.EqualFold(m.Get("EventList"), "Complete"):
		close(call.done)
	case call.isList:
		call.list = append(call.list, m)
	}
}

func (c *amiConn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err == io.EOF {
		err = errAMIClosed
	}
	c.err = err
	c.mu.Unlock()
	c.conn.Close()
	close(c.done)
	close(c.events)
}

// readMessage reads one "Key: Value" block terminated by an empty line.
func readMessage(r *bufio.Reader) (Message, error) {
	m := Message{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(m) == 0 {
				continue
			}
			return m, nil
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			// Continuation output from some legacy commands.
			k, v = "Output", line
		}
		v = strings.TrimPrefix(v, " ")
		if prev, dup := m[k]; dup {
			v = prev + "\n" + v
		}
		m[k] = v
	}
}
