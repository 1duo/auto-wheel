package main

import (
	"encoding/json"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestStandardPolicyNativeCutoffAndReadiness(t *testing.T) {
	raw, c := nativeRetirement(t)
	var seal struct {
		At time.Time `json:"sealed_at"`
	}
	if err := json.Unmarshal(raw, &seal); err != nil {
		t.Fatal(err)
	}
	c.OrderCoveragePolicy = "exclusive_standard"
	c.ExclusiveOrdersSince = seal.At
	if _, _, err := verifyNativeSeal(raw, c); err != nil {
		t.Fatal(err)
	}
	c.ExclusiveOrdersSince = seal.At.Add(time.Second)
	if _, _, err := verifyNativeSeal(raw, c); err == nil {
		t.Fatal("later cutoff hid outside activity")
	}
	s := status{Config: c, LiveCapable: true, Broker: &wheel.Snapshot{At: time.Now(), OrderCoveragePolicy: "exclusive_standard"}, LiveReadiness: &liveReadiness{At: time.Now(), Ready: true}}
	if activationReady(s) {
		t.Fatal("undisclosed restricted activation admitted")
	}
	s.LiveReadiness.OrderCoveragePolicy = "exclusive_standard"
	s.LiveReadiness.CoverageWarning = wheel.StandardOrderWarning
	if !activationReady(s) {
		t.Fatal("qualified restricted activation rejected")
	}
}
