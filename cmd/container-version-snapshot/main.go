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

	"container-version-snapshot/pkg/snapshot"

	"github.com/kevinburke/ssh_config"
	"github.com/sethvargo/go-retry"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// HostResult holds the data collected from a host
type HostResult struct {
	Host       string
	Containers []snapshot.InspectedContainer
	Images     []snapshot.InspectedImage
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

func isTransientNetErr(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

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

func splitUserHost(hostAlias string) (string, string) {
	if strings.Contains(hostAlias, "@") {
		parts := strings.SplitN(hostAlias, "@", 2)
		return parts[0], parts[1]
	}
	return "", hostAlias
}

func collectFromHost(hostAlias string, client *ssh.Client) HostResult {
	res := HostResult{Host: hostAlias}

	ids, err := runSSHCommand(client, "podman ps -q")
	if err != nil {
		res.Error = fmt.Errorf("failed to list containers: %w", err)
		return res
	}
	idList := strings.Fields(ids)
	if len(idList) == 0 {
		return res
	}

	inspectCmd := "podman inspect " + strings.Join(idList, " ")
	inspectJSON, err := runSSHCommand(client, inspectCmd)
	if err != nil {
		res.Error = fmt.Errorf("failed to inspect containers: %w", err)
		return res
	}

	if err := json.Unmarshal([]byte(inspectJSON), &res.Containers); err != nil {
		res.Error = fmt.Errorf("failed to parse container json: %w", err)
		return res
	}

	imageIDs := make(map[string]bool)
	for _, c := range res.Containers {
		if c.ImageDigest != "" {
			imageIDs[c.ImageDigest] = true
		}
	}

	if len(imageIDs) > 0 {
		var imgArgs []string
		for id := range imageIDs {
			imgArgs = append(imgArgs, id)
		}
		imgInspectCmd := "podman image inspect " + strings.Join(imgArgs, " ")
		imgJSON, err := runSSHCommand(client, imgInspectCmd)
		if err != nil {
			res.Error = fmt.Errorf("failed to inspect images: %w", err)
			return res
		}
		if err := json.Unmarshal([]byte(imgJSON), &res.Images); err != nil {
			res.Error = fmt.Errorf("failed to parse image json: %w", err)
			return res
		}
	}

	return res
}

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

func printJSON(snap snapshot.Snapshot) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		log.Fatalf("Failed to encode JSON: %v", err)
	}
}

func main() {
	flag.Parse()
	hosts := flag.Args()

	if len(hosts) == 0 {
		log.Fatal("Usage: container-version-snapshot <host1> <host2> ...")
	}

	authMethods := getAuthMethods()
	registryClient := &snapshot.RealRegistryClient{}

	var wg sync.WaitGroup
	results := make(chan HostResult, len(hosts))

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			_, hostOnly := splitUserHost(h)
			client, err := createSSHClient(h, authMethods)
			if err != nil {
				results <- HostResult{Host: hostOnly, Error: err}
				return
			}
			defer func() { _ = client.Close() }()
			results <- collectFromHost(hostOnly, client)
		}(host)
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

	snap := snapshot.Snapshot{
		Timestamp:  time.Now(),
		Targets:    hosts,
		Containers: allRows,
	}

	printJSON(snap)
}
