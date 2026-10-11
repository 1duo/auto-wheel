package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"autowheel/internal/sharedclient"
	"autowheel/internal/wheel"
)

var sharedRelease = sync.OnceValues(sharedclient.ExecutableDigest)

func coordinatorClient(c wheel.Config) (sharedclient.Client, error) {
	release, err := sharedRelease()
	if err != nil {
		return sharedclient.Client{}, err
	}
	return sharedclient.Client{Socket: c.CoordinatorSocket, PolicyHash: c.CoordinatorPolicy, Release: release}, nil
}
func sharedStatus(c wheel.Config) (sharedclient.Status, error) {
	var status sharedclient.Status
	client, err := coordinatorClient(c)
	if err != nil {
		return status, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = client.Call(ctx, "status", map[string]any{}, &status); err != nil {
		return status, err
	}
	if status.AccountHash != sharedclient.Digest([]byte(c.Account)) || status.PolicyHash != c.CoordinatorPolicy || status.Release != client.Release {
		return status, errors.New("shared coordinator account/release/policy mismatch")
	}
	return status, nil
}
func sharedAccountReady(c wheel.Config) error {
	status, err := sharedStatus(c)
	if err != nil {
		return err
	}
	age := time.Since(status.At)
	if !status.Ready || !status.Reconciled || !status.SingleWriter || status.Generation <= 0 || age > 90*time.Second || age < -2*time.Second {
		if status.Reason != "" {
			return errors.New(status.Reason)
		}
		return errors.New("shared account cutover has not passed coordinator qualification")
	}
	return nil
}
func sharedPermission(c wheel.Config, enabled bool) (int64, error) {
	client, err := coordinatorClient(c)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var response struct {
		Applied bool  `json:"applied"`
		Epoch   int64 `json:"permission_epoch"`
	}
	err = client.Call(ctx, "permission", map[string]any{"new_risk": enabled, "management": false}, &response)
	if err != nil {
		return 0, err
	}
	if !response.Applied || response.Epoch <= 0 {
		return 0, errors.New("shared permission transition not acknowledged")
	}
	return response.Epoch, nil
}
