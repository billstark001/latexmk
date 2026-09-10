// Package httpapi exposes compilation, project, and administration endpoints.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	projectarchive "github.com/billstark001/latexmk/packages/server/internal/archive"
	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/jobs"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/resultarchive"
	"github.com/billstark001/latexmk/packages/server/internal/store"
)

// Server owns the Gin engine and exposes the v2 content-addressed upload and
// queued-job API. The legacy synchronous endpoint is opt-in.
type Server struct {
	cfg      config.Config
	meta     api.Metadata
	runner   *compile.Runner
	auth     *auth.Manager
	db       *store.Postgres
	projects *project.Manager
	jobs     *jobs.Manager
	logger   *slog.Logger
	engine   *gin.Engine
}

func New(
	cfg config.Config,
	meta api.Metadata,
	runner *compile.Runner,
	authManager *auth.Manager,
	db *store.Postgres,
	projects *project.Manager,
	queue *jobs.Manager,
	logger *slog.Logger,
) *Server {
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		cfg:      cfg,
		meta:     meta,
		runner:   runner,
		auth:     authManager,
		db:       db,
		projects: projects,
		jobs:     queue,
		logger:   logger,
	}
	engine := gin.New()
	engine.Use(s.recover(), s.requestID(), s.cors(), s.securityHeaders(), s.logRequests())
	engine.GET("/healthz", s.health)
	engine.GET("/readyz", s.ready)
	engine.GET("/v1/meta", s.metadata)

	compileAuth := authManager.Middleware(false)
	adminAuth := authManager.Middleware(true)
	if cfg.EnableLegacyCompile {
		engine.POST("/v1/compile", compileAuth, s.compileLegacy)
	}
	engine.POST("/v1/uploads/plans", compileAuth, s.planUpload)
	engine.PUT("/v1/uploads/:uploadID/blobs/:digest", compileAuth, s.putBlob)
	engine.POST("/v1/uploads/:uploadID/commit", compileAuth, s.commitUpload)
	engine.GET("/v1/jobs", compileAuth, s.listJobs)
	engine.GET("/v1/jobs/:id", compileAuth, s.getJob)
	engine.DELETE("/v1/jobs/:id", compileAuth, s.cancelJob)
	engine.GET("/v1/jobs/:id/result", compileAuth, s.downloadResult)
	engine.GET("/v1/projects/:id/cleanup", compileAuth, s.previewProjectCleanup)
	engine.DELETE("/v1/projects/:id/cleanup", compileAuth, s.cleanupProject)

	engine.GET("/v1/admin/users", adminAuth, s.listUsers)
	engine.POST("/v1/admin/users", adminAuth, s.createUser)
	engine.PATCH("/v1/admin/users/:id", adminAuth, s.patchUser)
	engine.POST("/v1/admin/users/:id/tokens", adminAuth, s.createToken)
	s.engine = engine
	return s
}

func (s *Server) Handler() http.Handler { return s.engine }

func (s *Server) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "latexmk"})
}

func (s *Server) ready(c *gin.Context) {
	if s.db != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			writeError(c, http.StatusServiceUnavailable, "database is not ready")
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

func (s *Server) metadata(c *gin.Context) {
	meta := s.meta
	meta.Timestamp = time.Now().UTC()
	c.JSON(http.StatusOK, meta)
}

// compileLegacy is available only when explicitly enabled for migrating v1
// clients. New clients use the queued protocol below.
func (s *Server) compileLegacy(c *gin.Context) {
	requestID := requestIDFrom(c.Request.Context())
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.MaxUploadBytes)
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeError(c, http.StatusUnsupportedMediaType, "Content-Type must be multipart/form-data")
		return
	}
	mr, err := c.Request.MultipartReader()
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid multipart request")
		return
	}
	jobWorkspace, err := compile.NewWorkspace(s.cfg.TempDir)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not create compile workspace")
		return
	}
	defer func() {
		if err := jobWorkspace.Close(); err != nil {
			s.logger.Warn("could not remove compile workspace", "error", err)
		}
	}()
	workspace := jobWorkspace.Project

	var request api.CompileRequest
	var gotRequest, gotProject bool
	for {
		part, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			writeError(c, http.StatusBadRequest, "invalid multipart body: "+nextErr.Error())
			return
		}
		switch part.FormName() {
		case "request":
			if gotRequest {
				_ = part.Close()
				writeError(c, http.StatusBadRequest, "duplicate request part")
				return
			}
			gotRequest = true
			if err := decodeStrictJSON(part, 64<<10, &request); err != nil {
				_ = part.Close()
				writeError(c, http.StatusBadRequest, "invalid compile request: "+err.Error())
				return
			}
		case "project":
			if gotProject {
				_ = part.Close()
				writeError(c, http.StatusBadRequest, "duplicate project part")
				return
			}
			gotProject = true
			if _, err := projectarchive.ExtractTarGz(
				part,
				workspace,
				projectarchive.Limits{MaxFiles: s.cfg.MaxFiles, MaxBytes: s.cfg.MaxExpandedBytes},
			); err != nil {
				_ = part.Close()
				writeError(c, http.StatusBadRequest, "invalid project archive: "+err.Error())
				return
			}
		default:
			_ = part.Close()
			writeError(c, http.StatusBadRequest, "unexpected multipart field "+part.FormName())
			return
		}
		_ = part.Close()
	}
	if !gotRequest || !gotProject {
		writeError(c, http.StatusBadRequest, "request and project parts are required")
		return
	}
	if request.Auxiliary.Server == "reuse" {
		writeError(c, http.StatusBadRequest, "server compile cache requires the queued compilation API")
		return
	}
	if err := s.runner.Validate(workspace, request); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	s.logger.Info(
		"legacy compile started",
		"request_id",
		requestID,
		"user_id",
		principal.ID,
		"engine",
		request.Engine,
		"entry",
		request.Entry,
	)
	output := s.runner.Run(c.Request.Context(), workspace, request, requestID)
	if request.ProtocolVersion == 1 {
		// Preserve the result envelope expected by an unmodified v1 CLI.
		output.Result.ProtocolVersion = 1
	}
	output.Result.ServerVersion = s.meta.Version
	output.Result.ImageProfile = s.meta.ImageProfile
	responsePath := filepath.Join(jobWorkspace.Path, "result.tar.gz")
	output = compile.RetainArtifacts(output, request, s.cfg.ResultRetention)
	if err := resultarchive.Write(responsePath, output); err != nil {
		writeError(c, http.StatusInternalServerError, "could not package compile result")
		return
	}
	f, err := os.Open(responsePath)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not read compile result")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not stat compile result")
		return
	}
	c.Header("Content-Type", "application/vnd.latexmk.result+tar.gz")
	c.Header("Content-Disposition", `attachment; filename="latexmk-result.tar.gz"`)
	c.Header("Content-Length", strconv.FormatInt(st.Size(), 10))
	c.Header("X-Latexmk-Request-ID", requestID)
	c.Header("X-Latexmk-Server-Version", s.meta.Version)
	c.Header("X-Latexmk-Image-Profile", s.meta.ImageProfile)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

func (s *Server) planUpload(c *gin.Context) {
	var request api.UploadPlanRequest
	if err := decodeStrictJSON(c.Request.Body, 4<<20, &request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid upload plan: "+err.Error())
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	if err := s.runner.ValidateRequest(request.Request); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := s.projects.Plan(principal.ID, request)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusCreated, plan)
}

func (s *Server) putBlob(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.MaxUploadBytes)
	if err := s.projects.PutBlob(principal.ID, c.Param("uploadID"), c.Param("digest"), c.Request.Body); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) commitUpload(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	snapshot, request, err := s.projects.Commit(c.Request.Context(), principal.ID, c.Param("uploadID"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.jobs.Enqueue(c.Request.Context(), principal.ID, snapshot, request)
	if err != nil {
		writeError(c, http.StatusTooManyRequests, err.Error())
		return
	}
	c.Header("Location", "/v1/jobs/"+job.ID)
	c.JSON(http.StatusAccepted, job)
}

func (s *Server) listJobs(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	limit, _ := strconv.Atoi(c.Query("limit"))
	jobs, err := s.jobs.List(c.Request.Context(), principal.ID, limit)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not list jobs")
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

func (s *Server) getJob(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	job, err := s.jobs.Get(c.Request.Context(), principal.ID, c.Param("id"))
	if err != nil {
		writeError(c, http.StatusNotFound, "job not found")
		return
	}
	c.JSON(http.StatusOK, job)
}

func (s *Server) cancelJob(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	job, err := s.jobs.Cancel(c.Request.Context(), principal.ID, c.Param("id"))
	if err != nil {
		writeError(c, http.StatusConflict, err.Error())
		return
	}
	c.JSON(http.StatusOK, job)
}

func (s *Server) downloadResult(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	path, _, err := s.jobs.ResultPath(c.Request.Context(), principal.ID, c.Param("id"))
	if err != nil {
		writeError(c, http.StatusConflict, err.Error())
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(c, http.StatusNotFound, "job result archive is unavailable")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not read job result")
		return
	}
	c.Header("Content-Type", "application/vnd.latexmk.result+tar.gz")
	c.Header("Content-Disposition", `attachment; filename="latexmk-result.tar.gz"`)
	c.DataFromReader(http.StatusOK, st.Size(), "application/vnd.latexmk.result+tar.gz", f, nil)
}

func (s *Server) previewProjectCleanup(c *gin.Context) {
	s.projectCleanup(c, true)
}

func (s *Server) cleanupProject(c *gin.Context) {
	s.projectCleanup(c, false)
}

func (s *Server) projectCleanup(c *gin.Context, dryRun bool) {
	scope := c.Query("scope")
	if scope == "" {
		writeError(c, http.StatusBadRequest, "cleanup scope is required")
		return
	}
	expectedDigest := c.Query("expectedDigest")
	if !dryRun {
		decoded, err := hex.DecodeString(expectedDigest)
		if err != nil || len(decoded) != sha256.Size {
			writeError(c, http.StatusBadRequest, "expectedDigest must be a 64-character SHA-256 digest on DELETE")
			return
		}
	} else if expectedDigest != "" {
		writeError(c, http.StatusBadRequest, "expectedDigest is only valid on DELETE")
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	var report api.CleanupReport
	var err error
	if dryRun {
		report, err = s.jobs.CleanupProject(c.Request.Context(), principal.ID, c.Param("id"), scope)
	} else {
		report, err = s.jobs.CleanupProjectWithPlan(
			c.Request.Context(),
			principal.ID,
			c.Param("id"),
			scope,
			expectedDigest,
		)
	}
	if err != nil {
		status := http.StatusConflict
		if !project.ValidProjectID(c.Param("id")) ||
			(scope != "results" && scope != "snapshot" && scope != "project" && scope != "cache") {
			status = http.StatusBadRequest
		}
		writeError(c, status, err.Error())
		return
	}
	c.JSON(http.StatusOK, report)
}

func (s *Server) listUsers(c *gin.Context) {
	if s.db == nil {
		writeError(c, http.StatusNotImplemented, "database user management is not enabled")
		return
	}
	users, err := s.db.ListUsers(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not list users")
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

func (s *Server) createUser(c *gin.Context) {
	if s.db == nil {
		writeError(c, http.StatusNotImplemented, "database user management is not enabled")
		return
	}
	var body struct{ Name, Email, Role string }
	if err := decodeStrictJSON(c.Request.Body, 64<<10, &body); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.db.CreateUser(c.Request.Context(), body.Name, body.Email, body.Role)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusCreated, user)
}

func (s *Server) patchUser(c *gin.Context) {
	if s.db == nil {
		writeError(c, http.StatusNotImplemented, "database user management is not enabled")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeStrictJSON(c.Request.Body, 64<<10, &body); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if body.Enabled == nil {
		writeError(c, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := s.db.SetUserEnabled(c.Request.Context(), c.Param("id"), *body.Enabled); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) createToken(c *gin.Context) {
	if s.db == nil {
		writeError(c, http.StatusNotImplemented, "database user management is not enabled")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeStrictJSON(c.Request.Body, 64<<10, &body); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	info, token, err := s.db.CreateToken(c.Request.Context(), c.Param("id"), body.Name)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"token": token, "tokenInfo": info})
}

func (s *Server) requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !validRequestID(id) {
			id = newRequestID()
		}
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		c.Next()
	}
}

func (s *Server) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Next()
	}
}

func (s *Server) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" || !s.allowedOrigin(origin) {
			c.Next()
			return
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Expose-Headers", "Location, X-Request-ID")
		c.Header("Access-Control-Max-Age", "600")
		c.Header("Vary", "Origin")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func (s *Server) allowedOrigin(origin string) bool {
	for _, allowed := range s.cfg.CORSOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

func (s *Server) logRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		if c.Request.URL.Path != "/healthz" {
			s.logger.Info(
				"http request",
				"request_id",
				requestIDFrom(c.Request.Context()),
				"method",
				c.Request.Method,
				"path",
				c.Request.URL.Path,
				"status",
				c.Writer.Status(),
				"duration_ms",
				time.Since(started).Milliseconds(),
			)
		}
	}
}

func (s *Server) recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error(
					"panic",
					"request_id",
					requestIDFrom(c.Request.Context()),
					"error",
					recovered,
					"stack",
					string(debug.Stack()),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
			}
		}()
		c.Next()
	}
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b)
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func decodeStrictJSON(r io.Reader, maxBytes int64, dst any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return fmt.Errorf("JSON body exceeds %d bytes", maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func writeError(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}
