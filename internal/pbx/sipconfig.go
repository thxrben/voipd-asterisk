// Package pbx ties the store, the engine and the call logic together.
package pbx

import (
	"context"
	"strconv"

	"voipd/internal/engine"
	"voipd/internal/store"
)

// BuildEngineConfig assembles the desired SIP configuration from the store.
// Disabled trunks and extensions are left out entirely.
func BuildEngineConfig(ctx context.Context, st *store.Store, net engine.Network) (engine.Config, error) {
	cfg := engine.Config{Network: net}

	trunks, err := st.Trunks(ctx)
	if err != nil {
		return cfg, err
	}
	for _, t := range trunks {
		if !t.Enabled {
			continue
		}
		et := engine.Trunk{
			Name:          t.Name,
			Profile:       t.Profile,
			Registrar:     t.Registrar,
			OutboundProxy: t.OutboundProxy,
			Transport:     t.Transport,
			AddressFamily: t.AddressFamily,
			AuthUsername:  t.AuthUsername,
			AuthPassword:  t.AuthPassword,
			SRTP:          t.SRTP,
			Codecs:        t.Codecs,
			Expiration:    t.RegExpiration,
		}
		for _, n := range t.Numbers {
			et.Lines = append(et.Lines, engine.TrunkLine{
				// The row id is stable across renames of the number.
				ID:       strconv.FormatInt(n.ID, 10),
				Number:   n.Number,
				SIPUser:  n.SIPUser,
				Register: n.Register,
			})
		}
		cfg.Trunks = append(cfg.Trunks, et)
	}

	exts, err := st.Extensions(ctx)
	if err != nil {
		return cfg, err
	}
	for _, e := range exts {
		if !e.Enabled {
			continue
		}
		cfg.Extensions = append(cfg.Extensions, engine.Extension{
			Number:      e.Number,
			Name:        e.Name,
			SIPUsername: e.SIPUsername,
			SIPPassword: e.SIPPassword,
			Transport:   e.Transport,
			SRTP:        e.SRTP,
			Codecs:      e.Codecs,
			MaxContacts: e.MaxContacts,
		})
	}
	return cfg, nil
}
