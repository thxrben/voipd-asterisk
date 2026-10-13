package store

import (
	"context"
	"testing"
)

func TestApplyPhoneBlockIsAtomic(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateListSource(ctx, ListSource{Name: "PB", Kind: KindPhoneBlock, Target: TargetBlock, IntervalMin: 1440, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	all := func(m []PhoneBlockNumber) []NewEntry {
		var out []NewEntry
		for _, n := range m {
			out = append(out, NewEntry{Pattern: n.Phone})
		}
		return out
	}
	res, err := st.ApplyPhoneBlock(ctx, id, true, []PhoneBlockNumber{{Phone: "+491", Votes: 4}, {Phone: "+492", Votes: 10}}, "5", all)
	if err != nil || res.Mirrored != 2 || res.Added != 2 {
		t.Fatalf("full: %+v %v", res, err)
	}

	// Failing half-way (here: cancelled while deriving) changes nothing.
	cctx, cancel := context.WithCancel(ctx)
	_, err = st.ApplyPhoneBlock(cctx, id, false, []PhoneBlockNumber{{Phone: "+491", Votes: 0}, {Phone: "+493", Votes: 2}}, "6",
		func(m []PhoneBlockNumber) []NewEntry { cancel(); return all(m) })
	if err == nil {
		t.Fatal("expected an error")
	}
	src, _ := st.ListSource(ctx, id)
	if n, _ := st.PhoneBlockCount(ctx, id); n != 2 || src.Cursor != "5" || src.Entries != 2 {
		t.Errorf("after failure: mirror %d cursor %q entries %d", n, src.Cursor, src.Entries)
	}

	// Incremental: removal and addition.
	res, err = st.ApplyPhoneBlock(ctx, id, false, []PhoneBlockNumber{{Phone: "+491", Votes: 0}, {Phone: "+493", Votes: 2}}, "6", all)
	if err != nil || res.Mirrored != 2 || res.Added != 1 || res.Removed != 1 {
		t.Errorf("incremental: %+v %v", res, err)
	}
	if err := st.DeleteListSource(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.PhoneBlockCount(ctx, id); n != 0 {
		t.Errorf("mirror not deleted with the source: %d", n)
	}
}
