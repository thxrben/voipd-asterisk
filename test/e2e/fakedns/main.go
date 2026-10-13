// Command fakedns is a tiny authoritative DNS server for the e2e tests.
// It answers from a zone file that is re-read on every query (tests change
// records by rewriting the file) and stops answering entirely while a
// "down" file exists (DNS outage).
//
// Zone file: one resource record per line in standard presentation format,
// e.g.
//
//	carrier.e2e. 1 IN NAPTR 10 50 "s" "SIP+D2U" "" _sip._udp.carrier.e2e.
//	_sip._udp.carrier.e2e. 1 IN SRV 10 50 5060 sbc1.carrier.e2e.
//	sbc1.carrier.e2e. 1 IN A 172.17.0.3
package main

import (
	"bufio"
	"flag"
	"log"
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
		rrs := load(*zone)
		for _, qq := range q.Question {
			nameKnown := false
			for _, rr := range rrs {
				h := rr.Header()
				if !strings.EqualFold(h.Name, qq.Name) {
					continue
				}
				nameKnown = true
				if h.Rrtype == qq.Qtype || qq.Qtype == dns.TypeANY {
					m.Answer = append(m.Answer, rr)
				}
			}
			if !nameKnown {
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

func load(path string) []dns.RR {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []dns.RR
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		rr, err := dns.NewRR(line)
		if err != nil {
			log.Printf("zone: %v", err)
			continue
		}
		out = append(out, rr)
	}
	return out
}
