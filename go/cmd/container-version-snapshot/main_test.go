package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"service-management/pkg/snapshot"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// MockFailRegistryClient simulates a registry client that returns errors.
type MockFailRegistryClient struct {
	Err error
}

func (m *MockFailRegistryClient) ListTags(_ context.Context, _ name.Repository) ([]string, error) {
	return nil, m.Err
}

func (m *MockFailRegistryClient) GetDigest(_ context.Context, _ name.Reference) (string, error) {
	return "", m.Err
}

func (m *MockFailRegistryClient) GetPlatformDigest(_ context.Context, _ name.Reference, _ *v1.Platform) (string, error) {
	return "", m.Err
}

func TestProcessHostData_ReturnsRemoteError(t *testing.T) {
	expectedErr := errors.New("registry connection failed")
	mockClient := &MockFailRegistryClient{Err: expectedErr}

	digest := "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	hostData := HostResult{
		Host: "test-host",
		Images: []snapshot.InspectedImage{
			{
				ID:          digest,
				RepoDigests: []string{"repo@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
				RepoTags:    []string{},
			},
		},
		Containers: []snapshot.InspectedContainer{
			{
				Name:        "/test-container",
				ImageDigest: digest,
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					ImageName: "repo:latest",
					Labels:    nil,
				},
			},
		},
	}

	rows := processHostData(context.Background(), hostData, mockClient)

	if len(rows) != 1 {
		t.Fatalf("Expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	if row.Error != expectedErr.Error() {
		t.Errorf("Expected Error to be %q, got %q", expectedErr.Error(), row.Error)
	}
}

func TestProcessHostData_HandlesMissingImage(t *testing.T) {
	mockClient := &MockFailRegistryClient{}

	digest := "sha256:missing"

	hostData := HostResult{
		Host:   "test-host",
		Images: []snapshot.InspectedImage{},
		Containers: []snapshot.InspectedContainer{
			{
				Name:        "/test-container",
				ImageDigest: digest,
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					ImageName: "repo:latest",
					Labels:    nil,
				},
			},
		},
	}

	rows := processHostData(context.Background(), hostData, mockClient)

	if len(rows) != 1 {
		t.Fatalf("Expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	expectedErr := "Image definition not found on host"
	if row.Error != expectedErr {
		t.Errorf("Expected Error to be %q, got %q", expectedErr, row.Error)
	}
}

func opErr(errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errno}
}

func TestIsTransientNetErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("something"), false},
		{"EHOSTUNREACH", opErr(syscall.EHOSTUNREACH), true},
		{"ENETUNREACH", opErr(syscall.ENETUNREACH), true},
		{"ECONNRESET", opErr(syscall.ECONNRESET), true},
		{"ECONNREFUSED", opErr(syscall.ECONNREFUSED), true},
		{"wrapped OpError", fmt.Errorf("outer: %w", opErr(syscall.EHOSTUNREACH)), true},
		{"ssh auth error", errors.New("ssh: handshake failed"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientNetErr(tc.err); got != tc.want {
				t.Errorf("isTransientNetErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBuildSnapshot(t *testing.T) {
	targets := []Target{
		{Alias: "user@nas", Sudo: false},
		{Alias: "admin@pie", Sudo: true},
	}

	rows := []snapshot.ContainerInfo{
		{Host: "pie", Container: "grafana"},
	}

	snap := buildSnapshot(targets, rows)

	if len(snap.Targets) != 2 {
		t.Fatalf("Expected 2 targets, got %d", len(snap.Targets))
	}

	if snap.Targets[0].Host != "nas" || snap.Targets[0].User != "user" || snap.Targets[0].Sudo != false {
		t.Errorf("Target 0 mismatch: %+v", snap.Targets[0])
	}

	if snap.Targets[1].Host != "pie" || snap.Targets[1].User != "admin" || snap.Targets[1].Sudo != true {
		t.Errorf("Target 1 mismatch: %+v", snap.Targets[1])
	}

	if len(snap.Containers) != 1 {
		t.Fatalf("Expected 1 container row, got %d", len(snap.Containers))
	}

	if snap.Timestamp.IsZero() {
		t.Errorf("Expected non-zero timestamp")
	}
}
