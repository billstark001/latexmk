package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/jobs"
	"github.com/gin-gonic/gin"
)

func (s *Server) createSession(c *gin.Context) {
	var req api.SessionRequest
	if err := decodeStrictJSON(c.Request.Body, 64<<10, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	session, err := s.jobs.CreateSession(c.Request.Context(), principal.ID, req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Header("Location", "/v1/sessions/"+session.ID)
	c.JSON(http.StatusCreated, session)
}

func (s *Server) getSession(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	session, err := s.jobs.GetSession(c.Request.Context(), principal.ID, c.Param("id"))
	if err != nil {
		writeError(c, http.StatusNotFound, err.Error())
		return
	}
	c.JSON(http.StatusOK, session)
}

func (s *Server) closeSession(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	if err := s.jobs.CloseSession(c.Request.Context(), principal.ID, c.Param("id")); err != nil {
		writeError(c, http.StatusNotFound, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) submitRevision(c *gin.Context) {
	var req api.RevisionRequest
	if err := decodeStrictJSON(c.Request.Body, 4096, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	job, err := s.jobs.SubmitRevision(c.Request.Context(), principal.ID, c.Param("id"), req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, jobs.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		if errors.Is(err, jobs.ErrRevisionConflict) {
			status = http.StatusConflict
		}
		writeError(c, status, err.Error())
		return
	}
	c.Header("Location", "/v1/jobs/"+job.ID)
	c.JSON(http.StatusAccepted, job)
}

func (s *Server) sessionEvents(c *gin.Context) {
	principal, _ := auth.FromContext(c.Request.Context())
	after := uint64(0)
	if value := c.GetHeader("Last-Event-ID"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid Last-Event-ID")
			return
		}
		after = parsed
	}
	if _, err := s.jobs.GetSession(c.Request.Context(), principal.ID, c.Param("id")); err != nil {
		writeError(c, http.StatusNotFound, err.Error())
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	controller := http.NewResponseController(c.Writer)
	if err := controller.Flush(); err != nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	// Force periodic reauthentication, including database token/user revocation.
	for {
		events, changed, err := s.jobs.SessionEvents(principal.ID, c.Param("id"), after)
		if err != nil {
			return
		}
		for _, event := range events {
			data, err := json.Marshal(event)
			if err != nil {
				return
			}
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if _, err := fmt.Fprintf(c.Writer, "id: %d\nevent: session\ndata: %s\n\n", event.Sequence, data); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			after = event.Sequence
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			renewed, err := s.auth.Authenticate(c.Request)
			if err != nil || renewed.ID != principal.ID {
				return
			}
			if _, err := s.jobs.GetSession(c.Request.Context(), principal.ID, c.Param("id")); err != nil {
				return
			}
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if _, err := fmt.Fprint(c.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}
