//go:build linux

// lcx-broker is a root-only service administrator entry, not a model tool.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/evalruntime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if os.Geteuid() != 0 {
		return errors.New("broker administration requires root")
	}
	if len(os.Args) < 3 {
		return errors.New("usage: lcx-broker serve|grant|revoke <policy.json> [pid|grant-id]")
	}
	path := os.Args[2]
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("policy path must be root-owned and not shared-writable")
		}
		if p == "/" {
			break
		}
		if !filepath.IsAbs(p) {
			return errors.New("absolute policy path required")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var policy evalruntime.BrokerPolicy
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err = dec.Decode(&policy); err != nil {
		return err
	}
	switch os.Args[1] {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return evalruntime.ServeBroker(ctx, policy)
	case "grant":
		if len(os.Args) != 7 {
			return errors.New("grant requires trusted client PID, cohort, job and trial IDs")
		}
		pid, err := strconv.Atoi(os.Args[3])
		if err != nil || pid <= 1 {
			return errors.New("invalid PID")
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return err
		}
		i := strings.LastIndex(string(raw), ")")
		if i < 0 {
			return errors.New("invalid process stat")
		}
		fields := strings.Fields(string(raw)[i+1:])
		if len(fields) < 20 {
			return errors.New("short process stat")
		}
		start, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil {
			return err
		}
		info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return err
		}
		id := hex.EncodeToString(nonce[:])
		grant := evalruntime.BrokerGrant{SchemaVersion: 1, ID: id, UID: int(st.Uid), GID: int(st.Gid), PID: pid, Starttime: start, ExpiresAt: time.Now().Add(30 * time.Minute), Boundary: evalapi.BoundaryContext{CohortID: os.Args[4], JobID: os.Args[5], TrialID: os.Args[6], ExecutionID: id, RequestID: "admission"}}
		if err := grant.Boundary.Validate(); err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(policy.Grants, id+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		err = json.NewEncoder(out).Encode(grant)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Println(id)
		return nil
	case "revoke":
		if len(os.Args) != 4 {
			return errors.New("revoke requires grant ID")
		}
		id := os.Args[3]
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			return errors.New("invalid grant ID")
		}
		for _, suffix := range []string{".json", ".json.consumed"} {
			if err = os.Remove(filepath.Join(policy.Grants, id+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	default:
		return errors.New("unsupported broker administrator command")
	}
}
