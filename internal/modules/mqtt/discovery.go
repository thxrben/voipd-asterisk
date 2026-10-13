package mqtt

import (
	"context"
	"encoding/json"

	"voipd/internal/store"
)

// Home Assistant MQTT discovery: one "voipd" device with switches for the
// blocking settings and DND, and sensors for lines, phones and calls.

type entity struct {
	component string // switch | binary_sensor | sensor
	id        string // object id, unique within voipd
	config    map[string]any
}

func (r *run) entities(exts []store.Extension) []entity {
	base := r.c.Base
	var out []entity
	toggle := func(id, name, state, command, icon string, on, off string) {
		cfg := map[string]any{"name": name, "state_topic": base + "/" + state, "icon": icon,
			"payload_on": "ON", "payload_off": "OFF", "state_on": "ON", "state_off": "OFF"}
		if r.c.Commands {
			cfg["command_topic"] = base + "/" + command
			cfg["payload_on"], cfg["payload_off"] = on, off
			out = append(out, entity{"switch", id, cfg})
			return
		}
		out = append(out, entity{"binary_sensor", id, cfg})
	}
	toggle("anonymous_block", "Block anonymous calls", "blocking/anonymous", "blocking/anonymous/set", "mdi:incognito", "ON", "OFF")
	toggle("blocking_paused", "Pause call blocking (30 min)", "blocking/paused", "blocking/pause/set", "mdi:shield-off", "30", "0")
	for _, e := range exts {
		toggle("dnd_"+e.Number, "Do not disturb "+e.Number+" "+e.Name, "extension/"+e.Number+"/dnd",
			"extension/"+e.Number+"/dnd/set", "mdi:phone-off", "ON", "OFF")
		out = append(out, entity{"binary_sensor", "registered_" + e.Number, map[string]any{
			"name": "Phone " + e.Number + " " + e.Name, "state_topic": base + "/extension/" + e.Number + "/registered",
			"payload_on": "ON", "payload_off": "OFF", "device_class": "connectivity", "entity_category": "diagnostic"}})
	}
	if r.h.Trunks != nil {
		for _, t := range r.h.Trunks.Statuses() {
			n := digits(t.Number)
			out = append(out, entity{"binary_sensor", "line_" + n, map[string]any{
				"name": "Line " + t.Number, "state_topic": base + "/line/" + n + "/state",
				"json_attributes_topic": base + "/line/" + n + "/attributes",
				"payload_on":            "up", "payload_off": "down", "device_class": "connectivity"}})
		}
	}
	stat := func(id, name, field, icon string) {
		out = append(out, entity{"sensor", id, map[string]any{"name": name, "state_topic": base + "/stats/today",
			"value_template": "{{ value_json." + field + " }}", "icon": icon, "state_class": "total"}})
	}
	stat("calls_today", "Calls today", "total", "mdi:phone")
	stat("missed_today", "Missed calls today", "missed", "mdi:phone-missed")
	stat("blocked_today", "Blocked calls today", "blocked", "mdi:phone-cancel")
	out = append(out,
		entity{"sensor", "active_calls", map[string]any{"name": "Active calls", "state_topic": base + "/calls/active",
			"icon": "mdi:phone-in-talk", "state_class": "measurement"}},
		entity{"sensor", "last_caller", map[string]any{"name": "Last caller", "state_topic": base + "/calls/last_caller",
			"icon": "mdi:phone-incoming"}},
	)
	return out
}

func (r *run) publishDiscovery(ctx context.Context, exts []store.Extension) {
	if !r.c.Discovery {
		return // stale configs are removed when they arrive (see handle)
	}
	device := map[string]any{"identifiers": []string{"voipd"}, "name": "voipd", "manufacturer": "voipd", "model": "Home PBX"}
	want := map[string]bool{}
	for _, e := range r.entities(exts) {
		topic := r.c.DiscoveryPrefix + "/" + e.component + "/voipd/" + e.id + "/config"
		want[topic] = true
		e.config["unique_id"] = "voipd_" + e.id
		e.config["object_id"] = "voipd_" + e.id
		e.config["device"] = device
		e.config["availability_topic"] = r.c.Base + "/status"
		b, _ := json.Marshal(e.config)
		if r.state["discovery:"+topic] == string(b) {
			continue
		}
		r.state["discovery:"+topic] = string(b)
		r.discovery[topic] = true
		r.publish(ctx, topic, string(b), true)
	}
	// Entities that went away (e.g. a deleted extension).
	for topic := range r.discovery {
		if !want[topic] {
			delete(r.discovery, topic)
			delete(r.state, "discovery:"+topic)
			r.publish(ctx, topic, "", true)
		}
	}
}
