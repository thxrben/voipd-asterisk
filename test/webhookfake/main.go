// Command webhookfake receives voipd webhooks for e2e tests. It checks each
// signature against the secret in -secret-file (read per request, so the
// test can write it after creating the endpoint) and appends one JSON line
// per request to -log: {"event", "valid", "body"}.
//
//	go run ./test/webhookfake -listen 127.0.0.1:18096 -secret-file s -log hooks.jsonl
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"voipd/internal/modules/webhook"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18096", "listen address")
	secretFile := flag.String("secret-file", "", "file with the endpoint secret")
	logFile := flag.String("log", "hooks.jsonl", "where to append received requests")
	flag.Parse()

	var mu sync.Mutex
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		secret, _ := os.ReadFile(*secretFile)
		want := webhook.Sign(strings.TrimSpace(string(secret)), r.Header.Get("X-Voipd-Timestamp"), body)
		line, _ := json.Marshal(map[string]any{
			"event": r.Header.Get("X-Voipd-Event"),
			"valid": r.Header.Get("X-Voipd-Signature") == want,
			"body":  json.RawMessage(body),
		})
		mu.Lock()
		f, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	log.Fatal(http.ListenAndServe(*listen, nil))
}
