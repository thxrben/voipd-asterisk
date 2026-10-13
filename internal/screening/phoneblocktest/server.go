// Package phoneblocktest is a fake PhoneBlock API for tests: /blocklist
// (full snapshot and changes since a version), /account, /blacklist,
// /whitelist and /test, behind a bearer token. Every change bumps the
// blocklist version, like the real service.
package phoneblocktest

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Entry is a blocklist number as the API returns it.
type Entry struct {
	Phone        string `json:"phone"`
	Rating       string `json:"rating"`
	Votes        int    `json:"votes"`
	LastActivity int64  `json:"lastActivity"`
}

type number struct {
	Entry
	changed int64 // version of the last change
}

type Server struct {
	token string

	mu       sync.Mutex
	version  int64
	nums     map[string]*number // removed numbers stay with votes 0
	black    []string
	white    []string
	minVotes int
	requests []string
	fail     []int // status codes for the next blocklist requests
}

func New(token string) *Server {
	return &Server{token: token, version: 1, nums: map[string]*number{}, minVotes: 4}
}

// Set adds or changes a number (votes 0 removes it).
func (s *Server) Set(phone string, votes int, rating string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.nums[phone] = &number{Entry{Phone: phone, Rating: rating, Votes: votes, LastActivity: s.version * 1000}, s.version}
}

// Bulk adds many numbers in one version (faster for big lists).
func (s *Server) Bulk(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	for _, e := range entries {
		s.nums[e.Phone] = &number{e, s.version}
	}
}

func (s *Server) Remove(phone string) { s.Set(phone, 0, "") }

// Clear removes every number without a trace (as if the list were reset).
func (s *Server) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.nums = map[string]*number{}
}

func (s *Server) SetPersonal(black, white []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.black, s.white = black, white
}

func (s *Server) SetMinVotes(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.minVotes = n
}

// FailNext makes the next blocklist requests answer with these codes.
func (s *Server) FailNext(codes ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, codes...)
}

// Requests lists the blocklist requests: "full" or "since=<v>".
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *Server) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/blocklist"):
		s.blocklist(w, r)
	case strings.HasSuffix(path, "/account"):
		writeJSON(w, map[string]any{"minVotes": s.minVotes, "wildcards": false, "lang": "de"})
	case strings.HasSuffix(path, "/blacklist"):
		writeJSON(w, map[string]any{"numbers": personal(s.black)})
	case strings.HasSuffix(path, "/whitelist"):
		writeJSON(w, map[string]any{"numbers": personal(s.white)})
	case strings.HasSuffix(path, "/test"):
		w.Write([]byte("ok"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) blocklist(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	if since == "" {
		s.requests = append(s.requests, "full")
	} else {
		s.requests = append(s.requests, "since="+since)
	}
	if len(s.fail) > 0 {
		code := s.fail[0]
		s.fail = s.fail[1:]
		http.Error(w, http.StatusText(code), code)
		return
	}
	var from int64 = -1
	if since != "" {
		v, err := strconv.ParseInt(since, 10, 64)
		if err != nil || v > s.version {
			http.Error(w, "invalid version", http.StatusBadRequest)
			return
		}
		from = v
	}
	out := []Entry{}
	for _, n := range s.nums {
		switch {
		case from < 0 && n.Votes >= 2: // full: active numbers only
			out = append(out, n.Entry)
		case from >= 0 && n.changed > from: // changes, removals with votes 0
			out = append(out, n.Entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Phone < out[j].Phone })
	writeJSON(w, map[string]any{"numbers": out, "version": s.version})
}

func personal(nums []string) []map[string]any {
	out := []map[string]any{}
	for _, n := range nums {
		out = append(out, map[string]any{"phone": n, "label": nil, "comment": nil, "rating": nil, "created": 0})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
