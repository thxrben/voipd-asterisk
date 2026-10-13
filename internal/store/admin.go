package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var (
	// ErrConflict: a unique value (name, number, username) already exists.
	ErrConflict = errors.New("already exists")
	// ErrInUse: the object is still referenced (e.g. a ring group by a route).
	ErrInUse = errors.New("still in use")
)

// constraintErr maps SQLite constraint failures to ErrConflict / ErrInUse.
func constraintErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return ErrConflict
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return ErrInUse
	}
	return err
}

func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return constraintErr(err)
	}
	return constraintErr(tx.Commit())
}

// --- trunks and numbers ---

func (s *Store) TrunkByID(ctx context.Context, id int64) (Trunk, error) {
	trunks, err := s.Trunks(ctx)
	if err != nil {
		return Trunk{}, err
	}
	for _, t := range trunks {
		if t.ID == id {
			return t, nil
		}
	}
	return Trunk{}, ErrNotFound
}

func (s *Store) CreateTrunk(ctx context.Context, t Trunk) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO trunks(name, enabled, profile, registrar, outbound_proxy, transport, address_family,
		                   auth_username, auth_password, srtp, dial_format, codecs, reg_expiration)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Enabled, t.Profile, t.Registrar, t.OutboundProxy, t.Transport, t.AddressFamily,
		t.AuthUsername, t.AuthPassword, t.SRTP, t.DialFormat, strings.Join(t.Codecs, ","), t.RegExpiration)
	if err != nil {
		return 0, constraintErr(err)
	}
	return res.LastInsertId()
}

// UpdateTrunk writes all fields; the password only if setPassword.
func (s *Store) UpdateTrunk(ctx context.Context, t Trunk, setPassword bool) error {
	q := `UPDATE trunks SET name = ?, enabled = ?, profile = ?, registrar = ?, outbound_proxy = ?, transport = ?,
	        address_family = ?, auth_username = ?, srtp = ?, dial_format = ?, codecs = ?, reg_expiration = ?`
	args := []any{t.Name, t.Enabled, t.Profile, t.Registrar, t.OutboundProxy, t.Transport, t.AddressFamily,
		t.AuthUsername, t.SRTP, t.DialFormat, strings.Join(t.Codecs, ","), t.RegExpiration}
	if setPassword {
		q += `, auth_password = ?`
		args = append(args, t.AuthPassword)
	}
	q += ` WHERE id = ?`
	args = append(args, t.ID)
	return affected(s.DB.ExecContext(ctx, q, args...))
}

func (s *Store) DeleteTrunk(ctx context.Context, id int64) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM trunks WHERE id = ?`, id))
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return constraintErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) NumberByID(ctx context.Context, id int64) (TrunkNumber, error) {
	var n TrunkNumber
	err := s.DB.QueryRowContext(ctx, `SELECT id, trunk_id, number, sip_user, register, label FROM trunk_numbers WHERE id = ?`,
		id).Scan(&n.ID, &n.TrunkID, &n.Number, &n.SIPUser, &n.Register, &n.Label)
	return n, notFound(err)
}

func (s *Store) CreateNumber(ctx context.Context, n TrunkNumber) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO trunk_numbers(trunk_id, number, sip_user, register, label) VALUES(?, ?, ?, ?, ?)`,
		n.TrunkID, n.Number, n.SIPUser, n.Register, n.Label)
	if err != nil {
		return 0, constraintErr(err)
	}
	return res.LastInsertId()
}

func (s *Store) UpdateNumber(ctx context.Context, n TrunkNumber) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE trunk_numbers SET number = ?, sip_user = ?, register = ?, label = ? WHERE id = ?`,
		n.Number, n.SIPUser, n.Register, n.Label, n.ID))
}

func (s *Store) DeleteNumber(ctx context.Context, id int64) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM trunk_numbers WHERE id = ?`, id))
}

// SetRoute routes a number to a ring group (groupID 0 removes the route).
func (s *Store) SetRoute(ctx context.Context, numberID, groupID int64) error {
	if groupID == 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM did_routes WHERE trunk_number_id = ?`, numberID)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO did_routes(trunk_number_id, ring_group_id) VALUES(?, ?)
		ON CONFLICT(trunk_number_id) DO UPDATE SET ring_group_id = excluded.ring_group_id`, numberID, groupID)
	return constraintErr(err)
}

// Routes maps trunk number ids to ring group ids.
func (s *Store) Routes(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT trunk_number_id, ring_group_id FROM did_routes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var n, g int64
		if err := rows.Scan(&n, &g); err != nil {
			return nil, err
		}
		out[n] = g
	}
	return out, rows.Err()
}

// --- extensions ---

func (s *Store) ExtensionByID(ctx context.Context, id int64) (Extension, error) {
	e, err := scanExtension(s.DB.QueryRowContext(ctx, `SELECT `+extensionCols+` FROM extensions e WHERE e.id = ?`, id))
	return e, notFound(err)
}

func nullable(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) CreateExtension(ctx context.Context, e Extension) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO extensions(number, name, enabled, sip_username, sip_password, transport, srtp, codecs,
		                       max_contacts, outbound_number_id, clir, dnd, owner_subject)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Number, e.Name, e.Enabled, e.SIPUsername, e.SIPPassword, e.Transport, e.SRTP, strings.Join(e.Codecs, ","),
		e.MaxContacts, nullable(e.OutboundNumberID), e.CLIR, e.DND, e.OwnerSubject)
	if err != nil {
		return 0, constraintErr(err)
	}
	return res.LastInsertId()
}

// UpdateExtension writes all fields; the SIP password only if setPassword.
func (s *Store) UpdateExtension(ctx context.Context, e Extension, setPassword bool) error {
	q := `UPDATE extensions SET number = ?, name = ?, enabled = ?, sip_username = ?, transport = ?, srtp = ?, codecs = ?,
	        max_contacts = ?, outbound_number_id = ?, clir = ?, dnd = ?, owner_subject = ?`
	args := []any{e.Number, e.Name, e.Enabled, e.SIPUsername, e.Transport, e.SRTP, strings.Join(e.Codecs, ","),
		e.MaxContacts, nullable(e.OutboundNumberID), e.CLIR, e.DND, e.OwnerSubject}
	if setPassword {
		q += `, sip_password = ?`
		args = append(args, e.SIPPassword)
	}
	q += ` WHERE id = ?`
	args = append(args, e.ID)
	return affected(s.DB.ExecContext(ctx, q, args...))
}

func (s *Store) SetDND(ctx context.Context, id int64, dnd bool) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE extensions SET dnd = ? WHERE id = ?`, dnd, id))
}

func (s *Store) DeleteExtension(ctx context.Context, id int64) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM extensions WHERE id = ?`, id))
}

// --- ring groups ---

type RingGroupConfig struct {
	ID                  int64   `json:"id"`
	Name                string  `json:"name"`
	RingTimeout         int     `json:"ring_timeout"`
	Fallback            string  `json:"fallback"`
	FallbackExtensionID int64   `json:"fallback_extension_id"`
	Members             []int64 `json:"members"` // extension ids
}

func (s *Store) RingGroups(ctx context.Context) ([]RingGroupConfig, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, ring_timeout, fallback, COALESCE(fallback_extension_id, 0) FROM ring_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RingGroupConfig
	idx := map[int64]int{}
	for rows.Next() {
		var g RingGroupConfig
		if err := rows.Scan(&g.ID, &g.Name, &g.RingTimeout, &g.Fallback, &g.FallbackExtensionID); err != nil {
			return nil, err
		}
		g.Members = []int64{}
		idx[g.ID] = len(out)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mrows, err := s.DB.QueryContext(ctx, `
		SELECT m.ring_group_id, m.extension_id FROM ring_group_members m JOIN extensions e ON e.id = m.extension_id
		ORDER BY e.number`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var g, e int64
		if err := mrows.Scan(&g, &e); err != nil {
			return nil, err
		}
		if i, ok := idx[g]; ok {
			out[i].Members = append(out[i].Members, e)
		}
	}
	return out, mrows.Err()
}

func (s *Store) SaveRingGroup(ctx context.Context, g RingGroupConfig) (int64, error) {
	id := g.ID
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if id == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO ring_groups(name, ring_timeout, fallback, fallback_extension_id) VALUES(?, ?, ?, ?)`,
				g.Name, g.RingTimeout, g.Fallback, nullable(g.FallbackExtensionID))
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
		} else {
			res, err := tx.ExecContext(ctx, `UPDATE ring_groups SET name = ?, ring_timeout = ?, fallback = ?, fallback_extension_id = ? WHERE id = ?`,
				g.Name, g.RingTimeout, g.Fallback, nullable(g.FallbackExtensionID), id)
			if err := affected(res, err); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM ring_group_members WHERE ring_group_id = ?`, id); err != nil {
				return err
			}
		}
		for _, m := range g.Members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ring_group_members(ring_group_id, extension_id) VALUES(?, ?)`, id, m); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

func (s *Store) DeleteRingGroup(ctx context.Context, id int64) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM ring_groups WHERE id = ?`, id))
}
