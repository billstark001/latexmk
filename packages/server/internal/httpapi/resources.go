package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/resources"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func (s *Server) resourceService(c *gin.Context) bool {
	if s.environments == nil {
		writeError(c, http.StatusServiceUnavailable, "resource environments are unavailable")
		return false
	}
	return true
}
func (s *Server) listEnvironments(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"baselines": s.environments.List(), "default": s.environments.DefaultID()})
}
func (s *Server) resolveEnvironment(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	var request model.ResolveRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	if err := decodeStrictJSON(c.Request.Body, 4<<20, &request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid resource environment: "+err.Error())
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	env, err := s.environments.Resolve(c.Request.Context(), principal.ID, request)
	if err != nil {
		resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, env)
}
func (s *Server) inspectEnvironment(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	env, err := s.environments.Inspect(principal.ID, c.Param("digest"))
	if err != nil {
		resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, env)
}
func (s *Server) putEnvironmentBlob(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	limit := s.cfg.MaxResourceBytes
	if limit == 0 {
		limit = 512 << 20
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	if err := s.environments.PutBlob(
		c.Request.Context(),
		principal.ID,
		c.Param("digest"),
		c.Param("sha"),
		c.Request.Body,
	); err != nil {
		resourceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
func (s *Server) prepareEnvironment(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	env, err := s.environments.Prepare(c.Request.Context(), principal.ID, c.Param("digest"))
	if err != nil {
		resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, env)
}
func (s *Server) exportEnvironmentBlob(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	file, metadata, err := s.environments.OpenExport(
		principal.ID,
		c.Param("digest"),
		c.Param("resourceID"),
		c.Param("sha"),
	)
	if err != nil {
		resourceError(c, err)
		return
	}
	defer func() { _ = file.Close() }()
	c.Header("X-Content-SHA256", metadata.SHA256)
	c.Header("Content-Length", strconv.FormatInt(metadata.Size, 10))
	c.DataFromReader(http.StatusOK, metadata.Size, "application/octet-stream", file, nil)
}
func resourceError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, resources.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, resources.ErrExport):
		status = http.StatusForbidden
	case errors.Is(err, resources.ErrQuota):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, resources.ErrNotReady):
		status = http.StatusConflict
	}
	writeError(c, status, err.Error())
}
