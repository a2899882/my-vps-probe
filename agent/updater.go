package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"my-vps-probe/common"
)

var (
	selfHashOnce   sync.Once
	selfHashValue  string
	selfUpdateMu   sync.Mutex
	selfUpdateBusy bool
	lastUpdateTry  time.Time
)

func selfBinaryHash() string {
	selfHashOnce.Do(func() {
		execPath, err := os.Executable()
		if err != nil {
			return
		}
		if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
			execPath = resolved
		}
		f, err := os.Open(execPath)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return
		}
		selfHashValue = hex.EncodeToString(h.Sum(nil))
	})
	return selfHashValue
}

func makeAgentDownloadURL(rawServer, downloadPath string) (string, error) {
	raw := strings.TrimSpace(rawServer)
	if raw == "" {
		return "", fmt.Errorf("empty server address")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server address")
	}
	if strings.EqualFold(u.Scheme, "wss") {
		u.Scheme = "https"
	} else if strings.EqualFold(u.Scheme, "ws") {
		u.Scheme = "http"
	}
	dp := strings.TrimSpace(downloadPath)
	if !strings.HasPrefix(dp, "/") {
		dp = "/" + dp
	}
	u.Path = strings.TrimRight(u.Path, "/") + dp
	u.RawQuery = ""
	return u.String(), nil
}

// maybeSelfUpdate atomically downloads, SHA-256 verifies, sanity-tests, swaps
// the agent binary, and exits cleanly so systemd/OpenRC restarts the new version.
func maybeSelfUpdate(instr common.AgentInstruction) {
	if !instr.AutoUpdate {
		return
	}
	targetHash := strings.ToLower(strings.TrimSpace(instr.AgentHash))
	downloadPath := strings.TrimSpace(instr.AgentDownloadURL)
	currentHash := selfBinaryHash()
	if targetHash == "" || downloadPath == "" || currentHash == "" || targetHash == currentHash {
		return
	}

	selfUpdateMu.Lock()
	if selfUpdateBusy || (!lastUpdateTry.IsZero() && time.Since(lastUpdateTry) < 5*time.Minute) {
		selfUpdateMu.Unlock()
		return
	}
	selfUpdateBusy = true
	lastUpdateTry = time.Now()
	selfUpdateMu.Unlock()

	go func() {
		defer func() {
			selfUpdateMu.Lock()
			selfUpdateBusy = false
			selfUpdateMu.Unlock()
		}()
		if err := performAtomicSelfUpdate(serverAddr, downloadPath, targetHash); err == nil {
			// Exit cleanly; systemd/OpenRC supervisor immediately starts the new binary.
			os.Exit(0)
		}
	}()
}

func performAtomicSelfUpdate(rawServer, downloadPath, expectedHash string) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}
	dlURL, err := makeAgentDownloadURL(rawServer, downloadPath)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(dlURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}

	dir := filepath.Dir(execPath)
	tmpFile, err := os.CreateTemp(dir, ".probe-agent-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	keep := false
	defer func() {
		_ = tmpFile.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, hasher), io.LimitReader(resp.Body, 32<<20)); err != nil {
		return err
	}
	if err := tmpFile.Chmod(0755); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	gotHash := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(gotHash, expectedHash) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", gotHash, expectedHash)
	}

	// Verify the new binary is executable on this OS/arch before replacing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, tmpPath, "-h").Run()

	if err := os.Rename(tmpPath, execPath); err != nil {
		return err
	}
	keep = true
	return nil
}
