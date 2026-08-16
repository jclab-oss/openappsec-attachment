package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// startRequest mirrors the JSON sent by the traefik plugin when a request starts.
type startRequest struct {
	ClientIP      string      `json:"clientIp"`
	ClientPort    uint16      `json:"clientPort"`
	ListeningIP   string      `json:"listeningIp"`
	ListeningPort uint16      `json:"listeningPort"`
	Protocol      string      `json:"protocol"`
	Method        string      `json:"method"`
	Host          string      `json:"host"`
	URI           string      `json:"uri"`
	Headers       [][2]string `json:"headers"`
	ContainsBody  bool        `json:"containsBody"`
}

type responseHeadersRequest struct {
	Code          int         `json:"code"`
	ContentLength uint64      `json:"contentLength"`
	Headers       [][2]string `json:"headers"`
}

type verdictReply struct {
	SessionID    uint32         `json:"sessionId,omitempty"`
	Verdict      Verdict        `json:"verdict"`
	Response     *BlockResponse `json:"response,omitempty"`
	Body         []byte         `json:"body,omitempty"`
	BodyModified bool           `json:"bodyModified,omitempty"`
}

const maxBodyChunkBytes = 64 * 1024 * 1024

// Server exposes the attachment manager over HTTP for the traefik plugin.
type Server struct {
	manager *AttachmentManager
	mux     *http.ServeMux
}

// NewServer builds the HTTP API in front of the given attachment manager.
func NewServer(manager *AttachmentManager) *Server {
	s := &Server{manager: manager, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /load-config", s.handleLoadConfig)
	s.mux.HandleFunc("POST /api/v1/session", s.handleStart)
	s.mux.HandleFunc("POST /api/v1/session/{id}/request-body", s.handleRequestBody)
	s.mux.HandleFunc("POST /api/v1/session/{id}/request-end", s.handleRequestEnd)
	s.mux.HandleFunc("POST /api/v1/session/{id}/response-headers", s.handleResponseHeaders)
	s.mux.HandleFunc("POST /api/v1/session/{id}/response-body", s.handleResponseBody)
	s.mux.HandleFunc("POST /api/v1/session/{id}/response-end", s.handleResponseEnd)
	s.mux.HandleFunc("DELETE /api/v1/session/{id}", s.handleFini)
	return s
}

// Serve listens on the given address. The address is either "host:port" or
// "unix:///path/to.sock".
func (s *Server) Serve(listenAddr string) error {
	var listener net.Listener
	var err error
	if strings.HasPrefix(listenAddr, "unix://") {
		path := strings.TrimPrefix(listenAddr, "unix://")
		listener, err = net.Listen("unix", path)
	} else {
		listener, err = net.Listen("tcp", listenAddr)
	}
	if err != nil {
		return err
	}
	log.Printf("openappsec traefik daemon listening on %s", listenAddr)
	return http.Serve(listener, s.mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("failed to encode response: %v", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ready": s.manager.Ready()})
}

func (s *Server) handleLoadConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.ReloadConfiguration(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sid, result := s.manager.StartTransaction(&StartTransactionData{
		ClientIP:      req.ClientIP,
		ClientPort:    req.ClientPort,
		ListeningIP:   req.ListeningIP,
		ListeningPort: req.ListeningPort,
		Protocol:      req.Protocol,
		Method:        req.Method,
		Host:          req.Host,
		URI:           req.URI,
		Headers:       req.Headers,
		ContainsBody:  req.ContainsBody,
	})
	writeJSON(w, http.StatusOK, verdictReply{
		SessionID: sid,
		Verdict:   result.Verdict,
		Response:  result.Block,
	})
}

func (s *Server) sessionID(w http.ResponseWriter, r *http.Request) (uint32, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session id"})
		return 0, false
	}
	return uint32(id), true
}

func (s *Server) handleBody(w http.ResponseWriter, r *http.Request, isRequest bool) {
	sid, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyChunkBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var result *InspectionResult
	if isRequest {
		result = s.manager.SendRequestBody(sid, body)
	} else {
		result = s.manager.SendResponseBody(sid, body)
	}
	writeJSON(w, http.StatusOK, verdictReply{
		Verdict:      result.Verdict,
		Response:     result.Block,
		Body:         result.ModifiedBody,
		BodyModified: result.BodyModified,
	})
}

func (s *Server) handleRequestBody(w http.ResponseWriter, r *http.Request) {
	s.handleBody(w, r, true)
}

func (s *Server) handleResponseBody(w http.ResponseWriter, r *http.Request) {
	s.handleBody(w, r, false)
}

func (s *Server) handleRequestEnd(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	result := s.manager.EndRequest(sid)
	writeJSON(w, http.StatusOK, verdictReply{Verdict: result.Verdict, Response: result.Block})
}

func (s *Server) handleResponseEnd(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	result := s.manager.EndResponse(sid)
	writeJSON(w, http.StatusOK, verdictReply{Verdict: result.Verdict, Response: result.Block})
}

func (s *Server) handleResponseHeaders(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	var req responseHeadersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result := s.manager.SendResponseHeaders(sid, &ResponseHeadersData{
		Code:          req.Code,
		ContentLength: req.ContentLength,
		Headers:       req.Headers,
	})
	writeJSON(w, http.StatusOK, verdictReply{Verdict: result.Verdict, Response: result.Block})
}

func (s *Server) handleFini(w http.ResponseWriter, r *http.Request) {
	sid, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	s.manager.FiniSession(sid)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
