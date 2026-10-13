// Command fakedns is a tiny authoritative DNS server for the e2e chaos
// tests. It answers A queries from a zone file that is re-read on every
// query (so tests change records by rewriting the file) with TTL 1, and
// stops answering entirely while a "down" file exists (DNS outage).
//
//	zone file lines: <name> <ipv4>
package main

import (
	"bufio"
	"flag"
	"log"
	"net"
	"os"
	"strings"

	"github.com/miekg/dns"
)

func main() {
	listen := flag.String("listen", ":53", "address to listen on (udp and tcp)")
	zone := flag.String("zone", "/data/zone", "zone file")
	down := flag.String("down", "/data/down", "while this file exists, queries are dropped")
	flag.Parse()

	dns.HandleFunc(".", func(w dns.ResponseWriter, q *dns.Msg) {
		if _, err := os.Stat(*down); err == nil {
			return // simulate an unreachable DNS server
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		records := load(*zone)
		for _, qq := range q.Question {
			ip, ok := records[strings.ToLower(qq.Name)]
			if !ok {
				continue
			}
			if qq.Qtype == dns.TypeA {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: qq.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1},
					A:   net.ParseIP(ip),
				})
			}
		}
		if len(m.Answer) == 0 && len(q.Question) > 0 {
			if _, ok := records[strings.ToLower(q.Question[0].Name)]; !ok {
				m.Rcode = dns.RcodeNameError
			}
		}
		w.WriteMsg(m)
	})
	for _, netw := range []string{"udp", "tcp"} {
		srv := &dns.Server{Addr: *listen, Net: netw}
		go func() { log.Fatal(srv.ListenAndServe()) }()
	}
	log.Printf("fakedns listening on %s", *listen)
	select {}
}

func load(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 {
			out[dns.Fqdn(strings.ToLower(fields[0]))] = fields[1]
		}
	}
	return out
}
