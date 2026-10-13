package screening

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"voipd/internal/numbers"
	"voipd/internal/store"
)

// PhoneBlockAPI is the default base URL of the PhoneBlock REST API.
const PhoneBlockAPI = "https://phoneblock.net/phoneblock/api"

// defaultMinVotes applies when the account setting cannot be read.
const defaultMinVotes = 4

// PhoneBlock's published vote buckets; a source's threshold is one of them
// (0 = the account's setting).
var PhoneBlockVotes = []int{2, 4, 10, 20, 50, 100}

var ratingLabel = map[string]string{
	"B_MISSED":      "Missed call",
	"C_PING":        "Ping call",
	"D_POLL":        "Poll",
	"E_ADVERTISING": "Advertising",
	"F_GAMBLE":      "Sweepstakes",
	"G_FRAUD":       "Fraud",
}

// phoneBlockLabel is the entry label, shown on phones as the caller name.
func phoneBlockLabel(n store.PhoneBlockNumber) string {
	if l := ratingLabel[n.Rating]; l != "" {
		return fmt.Sprintf("SPAM: %s (%d)", l, n.Votes)
	}
	return fmt.Sprintf("SPAM (%d)", n.Votes)
}

// errCursorRejected: PhoneBlock did not accept our "since" version.
var errCursorRejected = errors.New("PhoneBlock did not accept the last version; the next sync downloads the full list")

// syncPhoneBlock mirrors the PhoneBlock community blocklist: the full list
// on the first sync (PhoneBlock asks for at most one per month), only the
// changes since the last version afterwards (at most daily). The personal
// black- and whitelist of the account are applied on top.
func (s *Syncer) syncPhoneBlock(ctx context.Context, src store.ListSource) (SyncResult, error) {
	var res SyncResult
	base := strings.TrimRight(src.URL, "/")
	if base == "" {
		base = PhoneBlockAPI
	}
	api := &pbClient{http: s.http, base: base, key: src.Password}

	minVotes := src.MinVotes
	if minVotes == 0 {
		var acc struct {
			MinVotes int `json:"minVotes"`
		}
		if err := api.get(ctx, "/account", nil, &acc); err != nil {
			if errors.Is(err, errUnauthorized) {
				return res, err
			}
			s.log.Warn("phoneblock: account settings unavailable, using the default threshold", "err", err)
		}
		minVotes = acc.MinVotes
		if minVotes <= 0 {
			minVotes = defaultMinVotes
		}
	}

	full := src.Cursor == ""
	q := url.Values{}
	if !full {
		q.Set("since", src.Cursor)
	}
	nums, version, err := api.blocklist(ctx, q)
	if err != nil {
		var se statusError
		if !full && errors.As(err, &se) && (se.code == 400 || se.code == 404 || se.code == 410) {
			if cerr := s.st.SetCursor(context.WithoutCancel(ctx), src.ID, ""); cerr != nil {
				s.log.Warn("clear cursor", "err", cerr)
			}
			return res, fmt.Errorf("%w (%v)", errCursorRejected, err)
		}
		return res, err
	}
	if version == "" {
		return res, errors.New("PhoneBlock sent no version; keeping the previous list")
	}
	if full {
		have, err := s.st.PhoneBlockCount(ctx, src.ID)
		if err != nil {
			return res, err
		}
		if len(nums) == 0 || len(nums) < have/2 {
			return res, fmt.Errorf("PhoneBlock sent %d numbers instead of about %d; keeping the previous list", len(nums), have)
		}
	}

	var black, white struct {
		Numbers []struct {
			Phone string `json:"phone"`
		} `json:"numbers"`
	}
	if err := api.get(ctx, "/blacklist", nil, &black); err != nil {
		return res, fmt.Errorf("personal blocklist: %w", err)
	}
	if err := api.get(ctx, "/whitelist", nil, &white); err != nil {
		return res, fmt.Errorf("personal allowlist: %w", err)
	}

	derive := func(mirror []store.PhoneBlockNumber) []store.NewEntry {
		allowed := map[string]bool{}
		for _, n := range white.Numbers {
			if p := s.z.Parse(n.Phone); p.Kind == numbers.E164 {
				allowed[p.Value] = true
			}
		}
		seen := map[string]bool{}
		var out []store.NewEntry
		add := func(phone, label string) {
			p := s.z.Parse(phone)
			if p.Kind != numbers.E164 {
				res.Invalid++
				return
			}
			if allowed[p.Value] || seen[p.Value] {
				return
			}
			seen[p.Value] = true
			out = append(out, store.NewEntry{Pattern: p.Value, Label: label})
		}
		for _, n := range black.Numbers {
			add(n.Phone, "PhoneBlock: my blocklist")
		}
		for _, n := range mirror {
			if n.Votes >= minVotes {
				add(n.Phone, phoneBlockLabel(n))
			}
		}
		return out
	}
	r, err := s.st.ApplyPhoneBlock(ctx, src.ID, full, nums, version, derive)
	if err != nil {
		return SyncResult{}, err
	}
	res.Entries, res.Added, res.Removed = r.Entries, r.Added, r.Removed
	res.Cards, res.Full, res.Version = r.Mirrored, full, version
	return res, nil
}

var errUnauthorized = errors.New("PhoneBlock rejected the API key – create one in your PhoneBlock settings")

type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string { return fmt.Sprintf("PhoneBlock: HTTP %d %s", e.code, e.msg) }

type pbClient struct {
	http *http.Client
	base string
	key  string
}

func (c *pbClient) do(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "voipd")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		return nil, statusError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	return resp, nil
}

func (c *pbClient) get(ctx context.Context, path string, q url.Values, v any) error {
	resp, err := c.do(ctx, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// blocklist streams {"numbers": [...], "version": N}, so a big list is
// never held twice in memory.
func (c *pbClient) blocklist(ctx context.Context, q url.Values) ([]store.PhoneBlockNumber, string, error) {
	resp, err := c.do(ctx, "/blocklist", q)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	bad := func(err error) error { return fmt.Errorf("PhoneBlock blocklist: invalid JSON: %w", err) }
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, "", bad(fmt.Errorf("expected an object (%v)", err))
	}
	var nums []store.PhoneBlockNumber
	var version string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, "", bad(err)
		}
		switch t {
		case "numbers":
			if t, err := dec.Token(); err != nil || t != json.Delim('[') {
				return nil, "", bad(fmt.Errorf("numbers is not a list (%v)", err))
			}
			for dec.More() {
				var n store.PhoneBlockNumber
				if err := dec.Decode(&n); err != nil {
					return nil, "", bad(err)
				}
				nums = append(nums, n)
			}
			if _, err := dec.Token(); err != nil {
				return nil, "", bad(err)
			}
		case "version":
			var v json.Number
			if err := dec.Decode(&v); err != nil {
				return nil, "", bad(err)
			}
			if _, err := strconv.ParseInt(v.String(), 10, 64); err != nil {
				return nil, "", bad(fmt.Errorf("version %q", v))
			}
			version = v.String()
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, "", bad(err)
			}
		}
	}
	return nums, version, nil
}
