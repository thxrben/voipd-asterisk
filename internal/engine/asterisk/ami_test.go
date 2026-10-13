package asterisk

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeAMI accepts one connection and answers actions via handle. handle
// returns the raw packets to write back (already terminated).
func fakeAMI(t *testing.T, handle func(action Message) []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "Asterisk Call Manager/11.0.0\r\n")
		r := bufio.NewReader(conn)
		for {
			m, err := readMessage(r)
			if err != nil {
				return
			}
			for _, p := range handle(m) {
				fmt.Fprint(conn, p)
			}
		}
	}()
	return ln.Addr().String()
}

func pkt(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "%s: %s\r\n", kv[i], kv[i+1])
	}
	return b.String() + "\r\n"
}

// registrationsHandler answers Login (password "pw") and a registration
// list with an unrelated event interleaved, like real Asterisk does.
func registrationsHandler(a Message) []string {
	id := a.Get("ActionID")
	switch a.Get("Action") {
	case "Login":
		if a.Get("Secret") != "pw" {
			return []string{pkt("Response", "Error", "ActionID", id, "Message", "Authentication failed")}
		}
		return []string{pkt("Response", "Success", "ActionID", id)}
	case "PJSIPShowRegistrationsOutbound":
		return []string{
			pkt("Response", "Success", "ActionID", id, "EventList", "start"),
			pkt("Event", "ContactStatus", "URI", "sip:10@1.2.3.4", "ContactStatus", "Reachable"),
			pkt("Event", "OutboundRegistrationDetail", "ActionID", id, "ObjectName", "trunk-telekom-1", "Status", "Registered"),
			pkt("Event", "OutboundRegistrationDetail", "ActionID", id, "ObjectName", "trunk-telekom-2", "Status", "Rejected"),
			pkt("Event", "OutboundRegistrationDetailComplete", "ActionID", id, "EventList", "Complete"),
		}
	}
	return []string{pkt("Response", "Error", "ActionID", id, "Message", "Invalid/unknown command")}
}

func TestAMILoginRejected(t *testing.T) {
	addr := fakeAMI(t, registrationsHandler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dialAMI(ctx, addr, "voipd", "wrong"); err == nil {
		t.Fatal("expected login failure")
	}
}

func TestAMIEventList(t *testing.T) {
	addr := fakeAMI(t, registrationsHandler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialAMI(ctx, addr, "voipd", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, list, err := c.Action(ctx, "PJSIPShowRegistrationsOutbound")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Get("ObjectName") != "trunk-telekom-1" || list[1].Get("Status") != "Rejected" {
		t.Fatalf("unexpected list: %v", list)
	}
	select {
	case ev := <-c.Events():
		if ev.Get("Event") != "ContactStatus" {
			t.Fatalf("unexpected event %v", ev)
		}
	case <-ctx.Done():
		t.Fatal("interleaved event not delivered")
	}

	if _, _, err := c.Action(ctx, "Bogus"); err == nil || !strings.Contains(err.Error(), "Invalid/unknown") {
		t.Fatalf("expected error response, got %v", err)
	}
}

func TestAMIActionFailsWhenConnectionDrops(t *testing.T) {
	addr := fakeAMI(t, func(a Message) []string {
		if a.Get("Action") == "Login" {
			return []string{pkt("Response", "Success", "ActionID", a.Get("ActionID"))}
		}
		return nil // never answer
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialAMI(ctx, addr, "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.conn.Close() // simulate Asterisk going away mid-action
	}()
	if _, _, err := c.Action(ctx, "Ping"); err == nil {
		t.Fatal("expected error after connection drop")
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("Done not closed")
	}
}
