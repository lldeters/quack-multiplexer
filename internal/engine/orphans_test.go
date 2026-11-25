package engine_test

import (
	"context"
	"slices"
	"testing"

	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
)

// Only engines found at the start that shared sessions can have used are
// swept: not CONNECTION engines (warm ones included), not engines that are
// not ready, not engines this process launched.
func TestRecoveredTargets(t *testing.T) {
	prov := fake.New()
	groupKey := engine.Key{ShareLevel: engine.ShareGroup, Owner: "analytics", Subdomain: "default", Profile: "default"}
	serverKey := engine.Key{ShareLevel: engine.ShareServer, Subdomain: "default", Profile: "default"}
	connKey := engine.Key{ShareLevel: engine.ShareConnection, Owner: "s1", Profile: "default"}
	warmKey := engine.Key{ShareLevel: engine.ShareConnection, Profile: "default"}
	for id, key := range map[string]engine.Key{"user": userKey, "group": groupKey, "server": serverKey, "conn": connKey, "warm": warmKey} {
		defer prov.AddReady(id, key, "tok-"+id).Close()
	}
	list := &listOverride{Provider: prov}
	all, _ := prov.List(context.Background())
	for _, inst := range all {
		if inst.ID == "warm" {
			inst.Warm = true
		}
		list.list = append(list.list, inst)
	}
	starting := prov.AddReady("starting", engine.Key{ShareLevel: engine.ShareUser, Owner: "bob", Subdomain: "default", Profile: "default"}, "tok")
	defer starting.Close()
	list.list = append(list.list, engine.Instance{ID: "starting", Key: engine.Key{ShareLevel: engine.ShareUser, Owner: "bob",
		Subdomain: "default", Profile: "default"}, ProfileName: "default", Phase: engine.PhaseStarting, Token: "tok"})

	m := startManager(t, "", list, nil)
	var ids []string
	for _, tg := range m.RecoveredTargets() {
		if tg.Endpoint == "" || tg.Token == "" {
			t.Errorf("target %s without endpoint or token", tg.ID)
		}
		ids = append(ids, tg.ID)
	}
	if want := []string{"group", "server", "user"}; !slices.Equal(ids, want) {
		t.Fatalf("recovered targets %v, want %v", ids, want)
	}

	// An engine launched by this process carries no session of an earlier
	// one.
	lease, err := m.Acquire(context.Background(), engine.Key{ShareLevel: engine.ShareUser, Owner: "carol", Subdomain: "default", Profile: "default"}, "carol")
	if err != nil {
		t.Fatal(err)
	}
	lease.Commit()
	if n := len(m.RecoveredTargets()); n != 3 {
		t.Fatalf("%d recovered targets after a launch, want 3", n)
	}
}
