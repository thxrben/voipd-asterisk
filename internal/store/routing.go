package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrNotFound = errors.New("not found")

// Line is a trunk number together with what is needed to place calls on it.
type Line struct {
	TrunkNumber
	TrunkName  string
	DialFormat string
}

type RingGroup struct {
	ID          int64
	Name        string
	RingTimeout int // seconds
	Fallback    string
	// Members are the enabled extensions of the group (DND included; the
	// call flow decides how to treat DND).
	Members []Extension
}

const extensionCols = `e.id, e.number, e.name, e.enabled, e.sip_username, e.sip_password, e.transport, e.srtp,
	e.codecs, e.max_contacts, e.clir, e.dnd, COALESCE(e.outbound_number_id, 0), e.owner_subject`

func scanExtension(sc interface{ Scan(...any) error }) (Extension, error) {
	var e Extension
	var codecs string
	err := sc.Scan(&e.ID, &e.Number, &e.Name, &e.Enabled, &e.SIPUsername, &e.SIPPassword,
		&e.Transport, &e.SRTP, &codecs, &e.MaxContacts, &e.CLIR, &e.DND, &e.OutboundNumberID, &e.OwnerSubject)
	e.Codecs = splitCodecs(codecs)
	return e, err
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const lineCols = `n.id, n.trunk_id, n.number, n.sip_user, n.register, n.label, t.name, t.dial_format`

func scanLine(sc interface{ Scan(...any) error }) (Line, error) {
	var l Line
	err := sc.Scan(&l.ID, &l.TrunkID, &l.Number, &l.SIPUser, &l.Register, &l.Label, &l.TrunkName, &l.DialFormat)
	return l, notFound(err)
}

// LineByID returns an enabled trunk's number by its id.
func (s *Store) LineByID(ctx context.Context, id int64) (Line, error) {
	return scanLine(s.DB.QueryRowContext(ctx, `
		SELECT `+lineCols+` FROM trunk_numbers n JOIN trunks t ON t.id = n.trunk_id
		WHERE n.id = ? AND t.enabled`, id))
}

// DefaultOutboundLine returns the line used when an extension has none
// configured: the first number of the first enabled trunk.
func (s *Store) DefaultOutboundLine(ctx context.Context) (Line, error) {
	return scanLine(s.DB.QueryRowContext(ctx, `
		SELECT `+lineCols+` FROM trunk_numbers n JOIN trunks t ON t.id = n.trunk_id
		WHERE t.enabled ORDER BY t.name, n.id LIMIT 1`))
}

// OutboundLineFor returns the line an extension places external calls on.
func (s *Store) OutboundLineFor(ctx context.Context, extensionID int64) (Line, error) {
	var id sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT outbound_number_id FROM extensions WHERE id = ?`, extensionID).Scan(&id)
	if err != nil {
		return Line{}, notFound(err)
	}
	if id.Valid {
		if l, err := s.LineByID(ctx, id.Int64); err == nil {
			return l, nil
		}
	}
	return s.DefaultOutboundLine(ctx)
}

// RouteForLine returns the ring group a call to the given trunk number goes
// to, or ErrNotFound if the number has no route.
func (s *Store) RouteForLine(ctx context.Context, trunkNumberID int64) (RingGroup, error) {
	var g RingGroup
	err := s.DB.QueryRowContext(ctx, `
		SELECT g.id, g.name, g.ring_timeout, g.fallback
		FROM did_routes r JOIN ring_groups g ON g.id = r.ring_group_id
		WHERE r.trunk_number_id = ?`, trunkNumberID).Scan(&g.ID, &g.Name, &g.RingTimeout, &g.Fallback)
	if err != nil {
		return g, notFound(err)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT `+extensionCols+`
		FROM ring_group_members m JOIN extensions e ON e.id = m.extension_id
		WHERE m.ring_group_id = ? AND e.enabled ORDER BY e.number`, g.ID)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return g, err
		}
		g.Members = append(g.Members, e)
	}
	return g, rows.Err()
}

// ExtensionByNumber returns an enabled extension.
func (s *Store) ExtensionByNumber(ctx context.Context, number string) (Extension, error) {
	e, err := scanExtension(s.DB.QueryRowContext(ctx,
		`SELECT `+extensionCols+` FROM extensions e WHERE e.number = ? AND e.enabled`, number))
	return e, notFound(err)
}

// ExtensionBySIPUsername resolves a phone's engine endpoint id, which is
// its SIP username.
func (s *Store) ExtensionBySIPUsername(ctx context.Context, username string) (Extension, error) {
	e, err := scanExtension(s.DB.QueryRowContext(ctx,
		`SELECT `+extensionCols+` FROM extensions e WHERE e.sip_username = ? AND e.enabled`, username))
	return e, notFound(err)
}
