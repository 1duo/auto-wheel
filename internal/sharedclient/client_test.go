package sharedclient

import (
	"strings"
	"testing"
	"time"
)

func TestViewRequiresCompleteReleaseBoundProof(t *testing.T) {
	now := time.Now()
	c := Client{Release: strings.Repeat("a", 64), PolicyHash: strings.Repeat("b", 64)}
	v := View{FullJSON: `{"facts":[]}`, OwnedJSON: `{"positions":[]}`}
	v.Evidence = Evidence{Schema: "SharedAccountEvidenceV1", AccountHash: strings.Repeat("c", 64), PolicyHash: c.PolicyHash, Release: c.Release, FullHash: Digest([]byte(v.FullJSON)), OwnedHash: Digest([]byte(v.OwnedJSON)), At: now, Generation: 1, Epoch: 1, Reconciled: true, Qualified: true, SingleWriter: true}
	if err := c.ValidateView(v, v.Evidence.AccountHash, now); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*View){
		func(v *View) { v.Evidence.Reconciled = false },
		func(v *View) { v.Evidence.Qualified = false },
		func(v *View) { v.Evidence.SingleWriter = false },
		func(v *View) { v.Evidence.Release = strings.Repeat("d", 64) },
		func(v *View) { v.Evidence.At = now.Add(-31 * time.Second) },
		func(v *View) { v.OwnedJSON = `{"positions":["peer"]}` },
		func(v *View) { v.Evidence.Epoch = 0 },
	} {
		copy := v
		mutate(&copy)
		if err := c.ValidateView(copy, v.Evidence.AccountHash, now); err == nil {
			t.Fatal("unqualified shared view accepted")
		}
	}
}
