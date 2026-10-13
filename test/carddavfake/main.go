// Command carddavfake serves a PhoneBlock-like CardDAV address book for
// e2e tests: -n generated spam contacts (some with two numbers) plus the
// numbers given with -tel, behind basic auth.
//
//	go run ./test/carddavfake -listen 127.0.0.1:18095 -n 9000 -tel +491701234567
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"voipd/internal/screening/carddavtest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18095", "listen address")
	n := flag.Int("n", 9000, "number of generated contacts")
	tels := flag.String("tel", "", "comma-separated extra numbers to list")
	flag.Parse()

	s := carddavtest.New("phoneblock", "token", "/phoneblock/contacts/")
	for i := range *n {
		nums := []string{fmt.Sprintf("+4915%09d", i*7+1)}
		if i%5 == 0 {
			nums = append(nums, fmt.Sprintf("0%d %07d", 30+i%60, i))
		}
		s.Put(fmt.Sprintf("spam-%05d", i), fmt.Sprintf("SPAM: Werbung %d", i), nums...)
	}
	for i, t := range strings.Split(*tels, ",") {
		if t = strings.TrimSpace(t); t != "" {
			s.Put(fmt.Sprintf("extra-%d", i), "SPAM: e2e caller", t)
		}
	}
	log.Printf("serving %d contacts at http://%s%s", s.Len(), *listen, s.Home)
	log.Fatal(http.ListenAndServe(*listen, s))
}
