// Package store contains the PostgreSQL persistence layer.
//
// The name Postgres is kept for compatibility with the original server
// package, but the implementation deliberately talks the PostgreSQL wire
// protocol through GORM/pgx.  That makes a normal PostgreSQL service and a
// PGlite socket server interchangeable: both expose the same protocol.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type Postgres struct {
	db *gorm.DB
}

type User struct {
	ID        string    `gorm:"primaryKey;size:40" json:"id"`
	Name      string    `gorm:"not null" json:"name"`
	Email     string    `gorm:"not null;default:''" json:"email,omitempty"`
	Role      string    `gorm:"not null" json:"role"`
	Enabled   bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

type TokenInfo struct {
	ID        string    `gorm:"primaryKey;size:40" json:"id"`
	UserID    string    `gorm:"not null;index" json:"userId"`
	Name      string    `gorm:"not null" json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LastUsed  time.Time `json:"lastUsedAt,omitempty"`
}

type apiToken struct {
	ID        string `gorm:"primaryKey;size:40"`
	UserID    string `gorm:"not null;index"`
	Name      string `gorm:"not null"`
	TokenHash []byte `gorm:"type:bytea;not null;uniqueIndex"`
	CreatedAt time.Time
	LastUsed  *time.Time
}

func (User) TableName() string            { return "latexmk_users" }
func (apiToken) TableName() string        { return "latexmk_api_tokens" }
func (ProjectSnapshot) TableName() string { return "latexmk_project_snapshots" }
func (CompileJob) TableName() string      { return "latexmk_compile_jobs" }

// ProjectSnapshot holds the manifest only. Source bytes are content-addressed
// files on the server's state volume, which keeps the database small.
type ProjectSnapshot struct {
	ID        string `gorm:"primaryKey;size:40"`
	OwnerID   string `gorm:"not null;uniqueIndex:latexmk_snapshot_owner_project"`
	ProjectID string `gorm:"not null;uniqueIndex:latexmk_snapshot_owner_project;size:128"`
	Manifest  []byte `gorm:"type:bytea;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time `gorm:"index"`
}

// CompileJob stores the immutable source manifest required for an exact retry.
// Source bytes, logs, and compiled artifacts remain on the state volume.
type CompileJob struct {
	ID               string     `gorm:"primaryKey;size:40"`
	OwnerID          string     `gorm:"not null;index"`
	ProjectID        string     `gorm:"not null;index;size:128"`
	SnapshotID       string     `gorm:"index;size:40"`
	SnapshotManifest []byte     `gorm:"type:bytea"`
	Status           string     `gorm:"not null;index;size:16"`
	Request          []byte     `gorm:"type:bytea;not null"`
	Result           []byte     `gorm:"type:bytea"`
	Error            string     `gorm:"type:text"`
	ArchiveKey       string     `gorm:"size:255"`
	CreatedAt        time.Time  `gorm:"index"`
	StartedAt        *time.Time `gorm:"index"`
	FinishedAt       *time.Time `gorm:"index"`
}

func Open(ctx context.Context, databaseURL string) (*Postgres, error) {
	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Error),
	})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL connection: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	// PGlite is a single database connection behind a socket multiplexer. A
	// one-connection pool works for both PGlite and a full PostgreSQL instance
	// and avoids retaining idle server connections in a small lab deployment.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)
	p := &Postgres{db: db}
	if err := p.Ping(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := p.Migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() {
	if p == nil || p.db == nil {
		return
	}
	if sqlDB, err := p.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

func (p *Postgres) Ping(ctx context.Context) error {
	if p == nil || p.db == nil {
		return errors.New("database is unavailable")
	}
	return p.db.WithContext(ctx).Exec("SELECT 1").Error
}

func (p *Postgres) Migrate(ctx context.Context) error {
	if err := p.db.WithContext(ctx).AutoMigrate(&User{}, &apiToken{}, &ProjectSnapshot{}, &CompileJob{}); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := p.db.WithContext(ctx).Exec(`CREATE UNIQUE INDEX IF NOT EXISTS latexmk_users_email_unique ON latexmk_users (lower(email)) WHERE email <> ''`).Error; err != nil {
		return fmt.Errorf("create user email index: %w", err)
	}
	return nil
}

func (p *Postgres) AuthenticateToken(ctx context.Context, token string) (User, error) {
	hash := sha256.Sum256([]byte(token))
	var record apiToken
	if err := p.db.WithContext(ctx).Where("token_hash = ?", hash[:]).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, errors.New("invalid API token")
		}
		return User{}, fmt.Errorf("look up API token: %w", err)
	}
	var user User
	if err := p.db.WithContext(ctx).First(&user, "id = ?", record.UserID).Error; err != nil {
		return User{}, fmt.Errorf("look up token user: %w", err)
	}
	if !user.Enabled {
		return User{}, errors.New("user is disabled")
	}
	now := time.Now().UTC()
	_ = p.db.WithContext(ctx).Model(&apiToken{}).Where("id = ?", record.ID).Update("last_used", now).Error
	return user, nil
}

func (p *Postgres) CreateUser(ctx context.Context, name, email, role string) (User, error) {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	role = strings.ToLower(strings.TrimSpace(role))
	if name == "" {
		return User{}, errors.New("name is required")
	}
	if len(name) > 128 || len(email) > 254 {
		return User{}, errors.New("name must be at most 128 bytes and email at most 254 bytes")
	}
	if containsControl(name) || containsControl(email) {
		return User{}, errors.New("name and email may not contain control characters")
	}
	if role == "" {
		role = "member"
	}
	if role != "member" && role != "admin" {
		return User{}, errors.New("role must be admin or member")
	}
	id, err := randomID("usr")
	if err != nil {
		return User{}, err
	}
	user := User{ID: id, Name: name, Email: email, Role: role, Enabled: true}
	if err := p.db.WithContext(ctx).Create(&user).Error; err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

func (p *Postgres) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	if err := p.db.WithContext(ctx).Order("created_at ASC").Find(&users).Error; err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

func (p *Postgres) SetUserEnabled(ctx context.Context, id string, enabled bool) error {
	result := p.db.WithContext(ctx).Model(&User{}).Where("id = ?", id).Update("enabled", enabled)
	if result.Error != nil {
		return fmt.Errorf("set user enabled: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errors.New("user not found")
	}
	return nil
}

func (p *Postgres) CreateToken(ctx context.Context, userID, name string) (TokenInfo, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	if len(name) > 128 {
		return TokenInfo{}, "", errors.New("token name must be at most 128 bytes")
	}
	if containsControl(name) {
		return TokenInfo{}, "", errors.New("token name may not contain control characters")
	}
	var user User
	if err := p.db.WithContext(ctx).First(&user, "id = ?", userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TokenInfo{}, "", errors.New("user not found")
		}
		return TokenInfo{}, "", err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return TokenInfo{}, "", err
	}
	token := "lm_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	id, err := randomID("tok")
	if err != nil {
		return TokenInfo{}, "", err
	}
	record := apiToken{ID: id, UserID: userID, Name: name, TokenHash: hash[:]}
	if err := p.db.WithContext(ctx).Create(&record).Error; err != nil {
		return TokenInfo{}, "", fmt.Errorf("create API token: %w", err)
	}
	return TokenInfo{ID: record.ID, UserID: record.UserID, Name: record.Name, CreatedAt: record.CreatedAt}, token, nil
}

func (p *Postgres) SaveSnapshot(ctx context.Context, snapshot ProjectSnapshot) error {
	if snapshot.ID == "" {
		id, err := randomID("src")
		if err != nil {
			return err
		}
		snapshot.ID = id
	}
	return p.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_id"}, {Name: "project_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"manifest":   snapshot.Manifest,
			"updated_at": time.Now().UTC(),
		}),
	}).Create(&snapshot).Error
}

func (p *Postgres) LoadSnapshot(ctx context.Context, ownerID, projectID string) (ProjectSnapshot, error) {
	var snapshot ProjectSnapshot
	err := p.db.WithContext(ctx).Where("owner_id = ? AND project_id = ?", ownerID, projectID).First(&snapshot).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ProjectSnapshot{}, errors.New("project snapshot not found")
	}
	return snapshot, err
}

func (p *Postgres) ListSnapshots(ctx context.Context) ([]ProjectSnapshot, error) {
	var snapshots []ProjectSnapshot
	if err := p.db.WithContext(ctx).Find(&snapshots).Error; err != nil {
		return nil, err
	}
	return snapshots, nil
}

// VisitSnapshots keeps state-cache sweeping bounded when a database has many
// retained projects: manifests are decoded one batch at a time instead of being
// loaded into one large application slice.
func (p *Postgres) VisitSnapshots(ctx context.Context, batchSize int, visit func(ProjectSnapshot) error) error {
	if batchSize < 1 {
		batchSize = 100
	}
	var snapshots []ProjectSnapshot
	result := p.db.WithContext(ctx).Order("id ASC").FindInBatches(&snapshots, batchSize, func(_ *gorm.DB, _ int) error {
		for _, snapshot := range snapshots {
			if err := visit(snapshot); err != nil {
				return err
			}
		}
		return nil
	})
	return result.Error
}

func (p *Postgres) DeleteSnapshotsBefore(ctx context.Context, cutoff time.Time) error {
	return p.db.WithContext(ctx).Where("updated_at < ?", cutoff).Delete(&ProjectSnapshot{}).Error
}

func (p *Postgres) CreateJob(ctx context.Context, job CompileJob) error {
	return p.db.WithContext(ctx).Create(&job).Error
}

func (p *Postgres) GetJob(ctx context.Context, id string) (CompileJob, error) {
	var job CompileJob
	err := p.db.WithContext(ctx).First(&job, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CompileJob{}, errors.New("job not found")
	}
	return job, err
}

func (p *Postgres) ListJobs(ctx context.Context, ownerID string, limit int) ([]CompileJob, error) {
	var jobs []CompileJob
	query := p.db.WithContext(ctx).Order("created_at DESC").Limit(limit)
	if ownerID != "" {
		query = query.Where("owner_id = ?", ownerID)
	}
	if err := query.Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

func (p *Postgres) ListPendingJobs(ctx context.Context) ([]CompileJob, error) {
	var jobs []CompileJob
	if err := p.db.WithContext(ctx).Where("status IN ?", []string{"queued", "running"}).Order("created_at ASC").Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

// VisitActiveJobSnapshots exposes only immutable manifests referenced by jobs
// that may still need their source blobs after a server restart.
func (p *Postgres) VisitActiveJobSnapshots(ctx context.Context, batchSize int, visit func([]byte) error) error {
	if batchSize < 1 {
		batchSize = 100
	}
	var jobs []CompileJob
	result := p.db.WithContext(ctx).
		Select("id", "snapshot_manifest").
		Where("status IN ? AND snapshot_manifest IS NOT NULL", []string{"queued", "running"}).
		Order("id ASC").
		FindInBatches(&jobs, batchSize, func(_ *gorm.DB, _ int) error {
			for _, job := range jobs {
				if len(job.SnapshotManifest) == 0 {
					continue
				}
				if err := visit(job.SnapshotManifest); err != nil {
					return err
				}
			}
			return nil
		})
	return result.Error
}

func (p *Postgres) UpdateJob(ctx context.Context, id string, values map[string]any) error {
	return p.db.WithContext(ctx).Model(&CompileJob{}).Where("id = ?", id).Updates(values).Error
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}
