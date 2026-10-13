// Command oidcfake runs the test OIDC provider (internal/auth/oidctest) as
// a server for browser tests. It logs in whoever was last selected via
// POST /_test/user {"subject":"…","username":"…","groups":[…]}.
// Never expose it: it authenticates everyone.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"

	"voipd/internal/auth/oidctest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18090", "listen address")
	issuer := flag.String("issuer", "http://127.0.0.1:18090", "issuer URL (as clients reach it)")
	clientID := flag.String("client-id", "voipd", "client id")
	secret := flag.String("client-secret", "secret", "client secret")
	flag.Parse()

	p := oidctest.New(*clientID, *secret)
	p.Issuer = *issuer
	p.SetUser(oidctest.User{Subject: "admin", Username: "admin", Name: "Test Admin", Groups: []string{"voipd-admins"}})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /_test/user", func(w http.ResponseWriter, r *http.Request) {
		var u struct {
			Subject  string   `json:"subject"`
			Username string   `json:"username"`
			Name     string   `json:"name"`
			Email    string   `json:"email"`
			Groups   []string `json:"groups"`
		}
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.SetUser(oidctest.User{Subject: u.Subject, Username: u.Username, Name: u.Name, Email: u.Email, Groups: u.Groups})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", p)
	log.Printf("oidcfake: issuer %s on %s", *issuer, *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
