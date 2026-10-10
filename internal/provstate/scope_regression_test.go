package provstate

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCheckRouteDoesNotApplyOtherEvidenceScope(t *testing.T) {
	SetPathForTest(filepath.Join(t.TempDir(), "state.json"))
	Mark("p/m", State{Kind: "rejected", Scope: "transport", AccountID: "account-a", ModelID: "m", Transport: "responses", Until: time.Now().Add(time.Hour)})
	if _, bad := CheckRoute("p", "m", "responses", "account-a"); !bad {
		t.Fatal("matching restriction missed")
	}
	for _, args := range [][4]string{{"p", "m", "chat", "account-a"}, {"p", "m", "responses", "account-b"}, {"p", "n", "responses", "account-a"}} {
		if _, bad := CheckRoute(args[0], args[1], args[2], args[3]); bad {
			t.Fatalf("unrelated scope denied: %v", args)
		}
	}
}
