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

	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

const (
	cleanupPlanVersion = 1
	cleanupPlanTTL     = 10 * time.Minute
	maxCleanupPlans    = 64
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
	return filepath.Join(base, "latexmk", "cleanup-plans"), nil
}

func saveRemoteCleanupPlan(plan remoteCleanupPlan) error {
	dir, err := cleanupPlansDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("cleanup plan directory is not a real directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	active := 0
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if id == entry.Name() || !cleanupPlanIDPattern.MatchString(id) {
			continue
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if entryInfo.Mode().IsRegular() && time.Since(entryInfo.ModTime()) > cleanupPlanTTL+time.Minute {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		active++
	}
	if active >= maxCleanupPlans {
		return errors.New("too many active cleanup plans; wait for old plans to expire")
	}
	payload, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	path := filepath.Join(dir, plan.ID+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func loadRemoteCleanupPlan(planID string) (remoteCleanupPlan, string, error) {
	var plan remoteCleanupPlan
	if !cleanupPlanIDPattern.MatchString(planID) {
		return plan, "", errors.New("cleanup plan ID is invalid")
	}
	dir, err := cleanupPlansDir()
	if err != nil {
		return plan, "", err
	}
	path := filepath.Join(dir, planID+".json")
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return plan, path, errors.New("cleanup plan was not found; create a new preview")
		}
		return plan, path, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return plan, path, errors.New("cleanup plan file is invalid")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return plan, path, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, path, fmt.Errorf("parse cleanup plan: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return plan, path, errors.New("cleanup plan file contains trailing data")
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
		parsed.Host == "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
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
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return errors.New("remote cleanup plan was already consumed")
		}
		return err
	}
	return nil
}
