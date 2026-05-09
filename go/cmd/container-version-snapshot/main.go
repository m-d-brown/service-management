package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"service-management/pkg/snapshot"

	"github.com/kevinburke/ssh_config"
	"github.com/sethvargo/go-retry"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Target represents a host to scan and its configuration
type Target struct {
	// Alias is the SSH connection string (e.g., user@host)
	Alias string
	// Sudo indicates whether to run podman commands with elevated privileges
	Sudo bool
}

type stringSlice []string

// String returns a comma-separated representation of the string slice.
func (s *stringSlice) String() string {
	return strings.Join(*s, ", ")
}

// Set appends a value to the string slice, implementing the flag.Value interface.
func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// HostResult holds the data collected from a host
type HostResult struct {
	// Host is the address or alias of the target machine.
	Host       string
	// Containers holds metadata for all inspected containers on the host.
	Containers []snapshot.InspectedContainer
	// Images holds metadata for all inspected images on the host.
	Images     []snapshot.InspectedImage
	// Error captures any error encountered during collection.
	Error      error
}

// dialSSH resolves SSH config for hostAlias and makes a single dial attempt.
func dialSSH(hostAlias string, authMethods []ssh.AuthMethod) (*ssh.Client, string, error) {
	explicitUser, hostName := splitUserHost(hostAlias)

	hostname := ssh_config.Get(hostName, "Hostname")
	if hostname == "" {
		hostname = hostName
	}
	port := ssh_config.Get(hostName, "Port")
	if port == "" {
		port = "22"
	}
	user := ssh_config.Get(hostName, "User")
	if explicitUser != "" {
		user = explicitUser
	} else if user == "" {
		user = os.Getenv("USER")
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	target := net.JoinHostPort(hostname, port)
	client, err := ssh.Dial("tcp", target, config)
	return client, target, err
}

// isTransientNetErr determines if an error is a temporary network issue that might succeed on retry.
func isTransientNetErr(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// createSSHClient establishes an SSH connection to the specified host with retries.
func createSSHClient(hostAlias string, authMethods []ssh.AuthMethod) (*ssh.Client, error) {
	b := retry.WithMaxRetries(2, retry.WithJitterPercent(100, retry.NewExponential(3*time.Second)))

	var lastTarget string
	client, err := retry.DoValue(context.Background(), b, func(_ context.Context) (*ssh.Client, error) {
		c, target, err := dialSSH(hostAlias, authMethods)
		lastTarget = target
		if err != nil {
			if isTransientNetErr(err) {
				log.Printf("[%s] transient network error (will retry): %v", hostAlias, err)
				return nil, retry.RetryableError(err)
			}
			return nil, err
		}
		return c, nil
	})
	if err != nil {
		return nil, fmt.Errorf("ssh dial failed to %s (%s): %w", hostAlias, lastTarget, err)
	}
	return client, nil
}

// runSSHCommand executes a single shell command over an established SSH connection and returns its stdout.
func runSSHCommand(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer func() { _ = session.Close() }()

	var stdout bytes.Buffer
	session.Stdout = &stdout
	if err := session.Run(cmd); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// getAuthMethods discovers and configures SSH authentication methods (agent and keys).
func getAuthMethods() []ssh.AuthMethod {
	var methods []ssh.AuthMethod

	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			agentClient := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(agentClient.Signers))
		}
	}

	home, err := os.UserHomeDir()
	if err == nil {
		keyFiles := []string{
			filepath.Join(home, ".ssh", "id_rsa"),
			filepath.Join(home, ".ssh", "id_ed25519"),
		}
		for _, f := range keyFiles {
			key, err := os.ReadFile(f)
			if err == nil {
				signer, err := ssh.ParsePrivateKey(key)
				if err == nil {
					methods = append(methods, ssh.PublicKeys(signer))
				}
			}
		}
	}

	return methods
}

// splitUserHost separates an SSH alias into a username and hostname component.
func splitUserHost(hostAlias string) (string, string) {
	if strings.Contains(hostAlias, "@") {
		parts := strings.SplitN(hostAlias, "@", 2)
		return parts[0], parts[1]
	}
	return "", hostAlias
}

// collectFromContext gathers container and image information using the given SSH client.
// The sudo argument allows running podman commands with "sudo -n " prepended.
func collectFromContext(client *ssh.Client, sudo bool) ([]snapshot.InspectedContainer, []snapshot.InspectedImage, error) {
	prefix := ""
	if sudo {
		prefix = "sudo -n "
	}
	ids, err := runSSHCommand(client, prefix+"podman ps -q")
	if err != nil {
		return nil, nil, err
	}
	idList := strings.Fields(ids)
	if len(idList) == 0 {
		return nil, nil, nil
	}

	inspectCmd := prefix + "podman inspect " + strings.Join(idList, " ")
	inspectJSON, err := runSSHCommand(client, inspectCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to inspect containers: %w", err)
	}

	var containers []snapshot.InspectedContainer
	if err := json.Unmarshal([]byte(inspectJSON), &containers); err != nil {
		return nil, nil, fmt.Errorf("failed to parse container json: %w", err)
	}

	imageIDs := make(map[string]bool)
	for _, c := range containers {
		if c.ImageDigest != "" {
			imageIDs[c.ImageDigest] = true
		}
	}

	var images []snapshot.InspectedImage
	if len(imageIDs) > 0 {
		var imgArgs []string
		for id := range imageIDs {
			imgArgs = append(imgArgs, id)
		}
		imgInspectCmd := prefix + "podman image inspect " + strings.Join(imgArgs, " ")
		imgJSON, err := runSSHCommand(client, imgInspectCmd)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to inspect images: %w", err)
		}
		if err := json.Unmarshal([]byte(imgJSON), &images); err != nil {
			return nil, nil, fmt.Errorf("failed to parse image json: %w", err)
		}
	}

	return containers, images, nil
}

// collectFromHost orchestrates the collection of container and image data from a specific target host over SSH.
func collectFromHost(target Target, client *ssh.Client) HostResult {
	_, hostOnly := splitUserHost(target.Alias)
	res := HostResult{Host: hostOnly}

	containers, images, err := collectFromContext(client, target.Sudo)
	if err != nil {
		res.Error = fmt.Errorf("failed to list containers: %w", err)
		return res
	}

	res.Containers = containers
	res.Images = images

	return res
}

// processHostData transforms raw host collection data into structured container info rows, resolving versions if necessary.
func processHostData(ctx context.Context, data HostResult, client snapshot.RegistryClient) []snapshot.ContainerInfo {
	var rows []snapshot.ContainerInfo

	for _, c := range data.Containers {
		imgMeta, found := snapshot.FindImageWithDigest(data.Images, c.ImageDigest)

		var onHost snapshot.VersionDetails
		var remotePtr *snapshot.RemoteDetails
		var resolutionErr string

		if !found {
			resolutionErr = "Image definition not found on host"
		} else {
			onHost = snapshot.GetVersionFromInspection(c, imgMeta)
			hasLocal := onHost.OCI != "" || len(onHost.RepoTags) > 0 || len(onHost.Other) > 0
			if !hasLocal {
				v, err := snapshot.GetRemoteVersion(ctx, client, imgMeta)
				if err != nil {
					log.Printf("Failed to resolve version for %s (%s): %v", c.Config.ImageName, c.ImageDigest, err)
					resolutionErr = err.Error()
				} else if v != "" {
					remotePtr = &snapshot.RemoteDetails{Version: v}
				}
			}
		}

		name := strings.TrimPrefix(c.Name, "/")

		rows = append(rows, snapshot.ContainerInfo{
			Host:       data.Host,
			Container:  name,
			ImageLabel: c.Config.ImageName,
			OnHost:     onHost,
			Remote:     remotePtr,
			Error:      resolutionErr,
		})
	}
	return rows
}

// printJSON encodes and prints the final snapshot data to stdout.
func printJSON(snap snapshot.Snapshot) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		log.Fatalf("Failed to encode JSON: %v", err)
	}
}

// buildSnapshot constructs the final top-level Snapshot structure from targets and container infos.
func buildSnapshot(targets []Target, allRows []snapshot.ContainerInfo) snapshot.Snapshot {
	var targetInfos []snapshot.Target
	for _, t := range targets {
		explicitUser, hostOnly := splitUserHost(t.Alias)
		targetInfos = append(targetInfos, snapshot.Target{
			Host: hostOnly,
			User: explicitUser,
			Sudo: t.Sudo,
		})
	}

	return snapshot.Snapshot{
		Timestamp:  time.Now(),
		Targets:    targetInfos,
		Containers: allRows,
	}
}

func main() {
	var hosts stringSlice
	var sudoHosts stringSlice

	flag.Var(&hosts, "host", "Host to scan (can be specified multiple times)")
	flag.Var(&hosts, "t", "Alias for -host")
	flag.Var(&sudoHosts, "sudo-host", "Host to scan using sudo for podman (can be specified multiple times)")
	flag.Var(&sudoHosts, "s", "Alias for -sudo-host")

	flag.Parse()

	if len(hosts) == 0 && len(sudoHosts) == 0 {
		log.Fatal("Usage: container-version-snapshot [--host <host>] [--sudo-host <host>] ...")
	}

	var targets []Target
	for _, h := range hosts {
		targets = append(targets, Target{Alias: h, Sudo: false})
	}
	for _, h := range sudoHosts {
		targets = append(targets, Target{Alias: h, Sudo: true})
	}

	authMethods := getAuthMethods()
	registryClient := &snapshot.RealRegistryClient{}

	var wg sync.WaitGroup
	results := make(chan HostResult, len(targets))

	for _, target := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			_, hostOnly := splitUserHost(t.Alias)
			client, err := createSSHClient(t.Alias, authMethods)
			if err != nil {
				results <- HostResult{Host: hostOnly, Error: err}
				return
			}
			defer func() { _ = client.Close() }()
			results <- collectFromHost(t, client)
		}(target)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allRows []snapshot.ContainerInfo
	for res := range results {
		if res.Error != nil {
			allRows = append(allRows, snapshot.ContainerInfo{
				Host:  res.Host,
				Error: res.Error.Error(),
			})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rows := processHostData(ctx, res, registryClient)
		cancel()
		allRows = append(allRows, rows...)
	}

	sort.Slice(allRows, func(i, j int) bool {
		if allRows[i].Host != allRows[j].Host {
			return allRows[i].Host < allRows[j].Host
		}
		return allRows[i].Container < allRows[j].Container
	})

	snap := buildSnapshot(targets, allRows)

	printJSON(snap)

	if len(registryClient.CallCounts) > 0 {
		log.Printf("Registry External Call Totals:")
		for repo, count := range registryClient.CallCounts {
			log.Printf("  %s: %d", repo, count)
		}
	}
}
