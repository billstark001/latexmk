package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/shared/jsonutil"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const (
	cleanupPlanVersion   = 1
	cleanupPlanTTL       = 10 * time.Minute
	maxCleanupPlans      = 64
	maxCleanupPlanBytes  = 1 << 20
	cleanupPlanDirectory = "latexmk/cleanup-plans"
)

var cleanupPlanIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type remoteCleanupPlan struct {
	Version    int       `json:"version"`
	Kind       string    `json:"kind"`
	ID         string    `json:"planId"`
	Server     string    `json:"server"`
	ProjectID  string    `json:"projectId"`
	Scope      string    `json:"scope"`
	PlanDigest string    `json:"planDigest"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type remoteCleanupOutput struct {
	PlanID    string                 `json:"planId"`
	ExpiresAt *time.Time             `json:"expiresAt,omitempty"`
	Report    protocol.CleanupReport `json:"report"`
}

func createRemoteCleanupPlan(
	server, projectID, scope string,
	report protocol.CleanupReport,
) (remoteCleanupPlan, error) {
	if report.ProjectID != projectID || report.Scope != scope || !report.DryRun ||
		!validCleanupPlanDigest(report.PlanDigest) {
		return remoteCleanupPlan{}, errors.New("server returned an invalid cleanup preview")
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return remoteCleanupPlan{}, err
	}
	now := time.Now().UTC()
	plan := remoteCleanupPlan{
		Version:    cleanupPlanVersion,
		Kind:       "remote",
		ID:         hex.EncodeToString(idBytes),
		Server:     server,
		ProjectID:  projectID,
		Scope:      scope,
		PlanDigest: report.PlanDigest,
		CreatedAt:  now,
		ExpiresAt:  now.Add(cleanupPlanTTL),
	}
	if !validRemoteCleanupPlan(plan, plan.ID) {
		return remoteCleanupPlan{}, errors.New("remote cleanup plan contents are invalid")
	}
	if err := saveRemoteCleanupPlan(plan); err != nil {
		return remoteCleanupPlan{}, err
	}
	return plan, nil
}

func cleanupPlansDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.FromSlash(cleanupPlanDirectory)), nil
}

// openCleanupPlanRoot anchors operations at the user cache directory, so both
// the latexmk parent and plan directory remain confined during actual I/O.
func openCleanupPlanRoot(create bool) (*safefs.Root, string, error) {
	dir, err := cleanupPlansDir()
	if err != nil {
		return nil, "", err
	}
	base := filepath.Dir(filepath.Dir(dir))
	if create {
		if err := os.MkdirAll(base, 0700); err != nil {
			return nil, dir, err
		}
	}
	scoped, err := safefs.Open(base)
	if err != nil {
		return nil, dir, err
	}
	if create {
		if err := scoped.MakeDirs(cleanupPlanDirectory, 0700); err != nil {
			_ = scoped.Close()
			return nil, dir, err
		}
	}
	return scoped, dir, nil
}

func saveRemoteCleanupPlan(plan remoteCleanupPlan) error {
	if !validRemoteCleanupPlan(plan, plan.ID) {
		return errors.New("remote cleanup plan contents are invalid")
	}
	scoped, _, err := openCleanupPlanRoot(true)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	directory, err := scoped.Root.OpenRoot(cleanupPlanDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = entries.Close() }()
	active := 0
	for {
		batch, readErr := entries.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range batch {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if id == entry.Name() || !cleanupPlanIDPattern.MatchString(id) {
				continue
			}
			entryInfo, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if entryInfo.Mode().IsRegular() && time.Since(entryInfo.ModTime()) > cleanupPlanTTL+time.Minute {
				if err := directory.Remove(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
			active++
			if active >= maxCleanupPlans {
				return errors.New("too many active cleanup plans; wait for old plans to expire")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	payload, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	pending, err := scoped.Stage(
		cleanupPlanDirectory+"/"+plan.ID+".json",
		maxCleanupPlanBytes,
		func(w io.Writer) error { _, err := w.Write(payload); return err },
	)
	if err != nil {
		return err
	}
	defer func() { _ = pending.Close() }()
	return pending.CommitExclusive()
}

func loadRemoteCleanupPlan(planID string) (remoteCleanupPlan, string, error) {
	var plan remoteCleanupPlan
	if !cleanupPlanIDPattern.MatchString(planID) {
		return plan, "", errors.New("cleanup plan ID is invalid")
	}
	scoped, dir, err := openCleanupPlanRoot(false)
	path := filepath.Join(dir, planID+".json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return plan, path, errors.New("cleanup plan was not found; create a new preview")
		}
		return plan, path, err
	}
	defer func() { _ = scoped.Close() }()
	f, err := scoped.OpenRegular(cleanupPlanDirectory + "/" + planID + ".json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return plan, path, errors.New("cleanup plan was not found; create a new preview")
		}
		return plan, path, err
	}
	defer func() { _ = f.Close() }()
	if err := jsonutil.DecodeStrict(f, maxCleanupPlanBytes, &plan); err != nil {
		return plan, path, fmt.Errorf("parse cleanup plan: %w", err)
	}
	if !validRemoteCleanupPlan(plan, planID) {
		return plan, path, errors.New("remote cleanup plan contents are invalid")
	}
	return plan, path, nil
}

func validRemoteCleanupPlan(plan remoteCleanupPlan, id string) bool {
	if plan.Version != cleanupPlanVersion || plan.Kind != "remote" || plan.ID != id ||
		!validCleanupPlanDigest(plan.PlanDigest) {
		return false
	}
	parsed, err := url.Parse(plan.Server)
	if err != nil || plan.Server != strings.TrimRight(strings.TrimSpace(plan.Server), "/") ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Hostname() == "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(plan.Server, "#") ||
		parsed.Fragment != "" {
		return false
	}
	if plan.Scope != "results" && plan.Scope != "snapshot" && plan.Scope != "project" && plan.Scope != "cache" {
		return false
	}
	if !validRemoteCleanupProjectID(plan.ProjectID) || plan.CreatedAt.IsZero() || plan.ExpiresAt.IsZero() {
		return false
	}
	duration := plan.ExpiresAt.Sub(plan.CreatedAt)
	return duration > 0 && duration <= cleanupPlanTTL
}

func validRemoteCleanupProjectID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' ||
			r == '-' {
			continue
		}
		return false
	}
	return true
}

func validCleanupPlanDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func consumeRemoteCleanupPlan(path string) error {
	scoped, dir, err := openCleanupPlanRoot(false)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	name := filepath.Base(path)
	id := strings.TrimSuffix(name, ".json")
	if filepath.Dir(path) != dir || name != id+".json" || !cleanupPlanIDPattern.MatchString(id) {
		return errors.New("cleanup plan path is invalid")
	}
	if err := scoped.Remove(cleanupPlanDirectory + "/" + name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("remote cleanup plan was already consumed")
		}
		return err
	}
	return nil
}
