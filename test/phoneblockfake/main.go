// Command phoneblockfake serves a fake PhoneBlock API for e2e tests: -n
// generated spam numbers (mixed vote buckets) plus the numbers given with
// -tel (20 votes, fraud), behind the bearer token -token. Test control:
//
//	POST /_test/set?phone=+49…&votes=20&rating=G_FRAUD   (votes=0 removes)
//	GET  /_test/requests                                 blocklist requests, one per line
//
//	go run ./test/phoneblockfake -listen 127.0.0.1:18097 -n 30000 -tel +491701234567
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"voipd/internal/screening/phoneblocktest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18097", "listen address")
	token := flag.String("token", "pb-key", "API key")
	n := flag.Int("n", 30000, "number of generated numbers")
	tels := flag.String("tel", "", "comma-separated extra numbers to list")
	flag.Parse()

	s := phoneblocktest.New(*token)
	votes := []int{2, 4, 4, 10, 10, 20, 50, 100}
	ratings := []string{"E_ADVERTISING", "G_FRAUD", "C_PING", "D_POLL", "F_GAMBLE"}
	var bulk []phoneblocktest.Entry
	for i := range *n {
		bulk = append(bulk, phoneblocktest.Entry{Phone: fmt.Sprintf("+4916%08d", i*13+7), Votes: votes[i%len(votes)],
			Rating: ratings[i%len(ratings)], LastActivity: 1})
	}
	for _, t := range strings.Split(*tels, ",") {
		if t = strings.TrimSpace(t); t != "" {
			bulk = append(bulk, phoneblocktest.Entry{Phone: t, Votes: 20, Rating: "G_FRAUD", LastActivity: 1})
		}
	}
	s.Bulk(bulk)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /_test/set", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.URL.Query().Get("votes"))
		s.Set(r.URL.Query().Get("phone"), v, r.URL.Query().Get("rating"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_test/requests", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, strings.Join(s.Requests(), "\n"))
	})
	mux.Handle("/", s)
	log.Printf("fake PhoneBlock API on %s (%d numbers)", *listen, len(bulk))
	log.Fatal(http.ListenAndServe(*listen, mux))
}
