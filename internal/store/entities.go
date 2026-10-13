package store

import (
	"context"
	"strings"
)

type Trunk struct {
	ID            int64
	Name          string
	Enabled       bool
	Profile       string
	Registrar     string
	OutboundProxy string
	Transport     string
	AddressFamily string
	AuthUsername  string
	AuthPassword  string
	SRTP          bool
	DialFormat    string
	Codecs        []string
	RegExpiration int // seconds
	Numbers       []TrunkNumber
}

type TrunkNumber struct {
	ID       int64
	TrunkID  int64
	Number   string // E.164
	SIPUser  string
	Register bool
	Label    string
}

type Extension struct {
	ID          int64
	Number      string
	Name        string
	Enabled     bool
	SIPUsername string
	SIPPassword string
	Transport   string
	SRTP        bool
	Codecs      []string
	MaxContacts int
	CLIR        bool
	DND         bool
	// OutboundNumberID is the trunk number used for outgoing calls
	// (0 = first number of the first trunk).
	OutboundNumberID int64
	OwnerSubject     string // web UI user linked to this extension
}

func splitCodecs(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Trunks returns all trunks with their numbers, ordered by name.
func (s *Store) Trunks(ctx context.Context) ([]Trunk, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, enabled, profile, registrar, outbound_proxy, transport, address_family,
		       auth_username, auth_password, srtp, dial_format, codecs, reg_expiration
		FROM trunks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var trunks []Trunk
	byID := map[int64]int{}
	for rows.Next() {
		var t Trunk
		var codecs string
		if err := rows.Scan(&t.ID, &t.Name, &t.Enabled, &t.Profile, &t.Registrar, &t.OutboundProxy,
			&t.Transport, &t.AddressFamily, &t.AuthUsername, &t.AuthPassword, &t.SRTP, &t.DialFormat, &codecs,
			&t.RegExpiration); err != nil {
			return nil, err
		}
		t.Codecs = splitCodecs(codecs)
		byID[t.ID] = len(trunks)
		trunks = append(trunks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nrows, err := s.DB.QueryContext(ctx, `
		SELECT id, trunk_id, number, sip_user, register, label FROM trunk_numbers ORDER BY number`)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var n TrunkNumber
		if err := nrows.Scan(&n.ID, &n.TrunkID, &n.Number, &n.SIPUser, &n.Register, &n.Label); err != nil {
			return nil, err
		}
		if i, ok := byID[n.TrunkID]; ok {
			trunks[i].Numbers = append(trunks[i].Numbers, n)
		}
	}
	return trunks, nrows.Err()
}

// Extensions returns all extensions ordered by number.
func (s *Store) Extensions(ctx context.Context) ([]Extension, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+extensionCols+` FROM extensions e ORDER BY e.number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
