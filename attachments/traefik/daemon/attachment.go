package main

/*
#cgo CFLAGS: -D_GNU_SOURCE -I${SRCDIR}/../../../core/include/attachments -I${SRCDIR}/../../nano_attachment
#cgo LDFLAGS: -lnano_attachment -lnano_attachment_util -lshmem_ipc_2

#include <stdlib.h>
#include <string.h>
#include "nano_attachment_common.h"
#include "nano_attachment.h"

static HttpHeaderData* createHttpHeaderDataArray(size_t size) {
    return (HttpHeaderData*)calloc(size, sizeof(HttpHeaderData));
}

static void setHeaderElement(HttpHeaderData* arr, size_t index, nano_str_t key, nano_str_t value) {
    if (arr == NULL) {
        return;
    }
    arr[index].key = key;
    arr[index].value = value;
}

static nano_str_t* createNanoStrArray(size_t size) {
    return (nano_str_t*)calloc(size, sizeof(nano_str_t));
}

static void setNanoStrElement(nano_str_t* arr, size_t index, nano_str_t value) {
    if (arr == NULL) {
        return;
    }
    arr[index] = value;
}
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Verdict is the decision returned to the traefik plugin for each inspected chunk.
type Verdict string

const (
	// VerdictInspect means the chunk was accepted so far and inspection must continue.
	VerdictInspect Verdict = "inspect"
	// VerdictAccept is a final verdict; the remainder of the session is not inspected.
	VerdictAccept Verdict = "accept"
	// VerdictDrop is a final verdict; the transaction must be blocked.
	VerdictDrop Verdict = "drop"
	// VerdictNoop means inspection is not possible (attachment not ready); fail-open.
	VerdictNoop Verdict = "noop"
)

const bodyChunkSize = 8 * 1024

// BlockResponse describes what should be returned to the client on a drop verdict.
type BlockResponse struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// InspectionResult is the outcome of sending one chunk to the nano agent.
type InspectionResult struct {
	Verdict      Verdict
	Block        *BlockResponse
	ModifiedBody []byte
	BodyModified bool
}

// StartTransactionData carries the request metadata of a new session.
type StartTransactionData struct {
	ClientIP      string
	ClientPort    uint16
	ListeningIP   string
	ListeningPort uint16
	Protocol      string
	Method        string
	Host          string
	URI           string
	Headers       [][2]string
	ContainsBody  bool
}

// ResponseHeadersData carries the upstream response headers of a session.
type ResponseHeadersData struct {
	Code          int
	ContentLength uint64
	Headers       [][2]string
}

type worker struct {
	mu         sync.Mutex
	attachment *C.NanoAttachment
	ready      atomic.Bool
}

type session struct {
	id         uint32
	worker     *worker
	data       *C.HttpSessionData
	mu         sync.Mutex
	lastActive atomic.Int64
}

func (s *session) touch() {
	s.lastActive.Store(time.Now().UnixNano())
}

// AttachmentManager owns the nano attachment instances and the active sessions.
type AttachmentManager struct {
	workers       []*worker
	sessionsMu    sync.RWMutex
	sessions      map[uint32]*session
	nextSessionID atomic.Uint32
	sessionTTL    time.Duration

	stats Stats
}

// Stats counts the inspection work done, which is what makes two deployments
// comparable: a transaction the agent finalizes on the first call costs one
// round trip, while one it keeps inspecting costs a call per body chunk and
// per response stage.
type Stats struct {
	// TransactionsStarted counts calls that opened a transaction.
	TransactionsStarted atomic.Uint64
	// TransactionsInspected counts transactions the agent did not finalize
	// immediately, i.e. the ones that were inspected beyond their first call.
	TransactionsInspected atomic.Uint64
	// ChunksSent counts every chunk handed to the agent.
	ChunksSent atomic.Uint64
}

// Snapshot returns the current counters.
func (m *AttachmentManager) Snapshot() map[string]uint64 {
	return map[string]uint64{
		"transactionsStarted":   m.stats.TransactionsStarted.Load(),
		"transactionsInspected": m.stats.TransactionsInspected.Load(),
		"chunksSent":            m.stats.ChunksSent.Load(),
	}
}

// NewAttachmentManager creates a manager with numWorkers attachment instances.
// The attachments are initialized in the background; until an attachment is
// ready its sessions get a "noop" verdict (fail-open).
func NewAttachmentManager(numWorkers int, sessionTTL time.Duration) *AttachmentManager {
	m := &AttachmentManager{
		workers:    make([]*worker, numWorkers),
		sessions:   make(map[uint32]*session),
		sessionTTL: sessionTTL,
	}
	for i := range m.workers {
		m.workers[i] = &worker{}
	}
	return m
}

// Init keeps retrying the registration of every attachment instance with the
// nano agent until it succeeds.
func (m *AttachmentManager) Init() {
	numWorkers := len(m.workers)
	for i, w := range m.workers {
		for {
			attachment := C.InitNanoAttachment(
				C.uint8_t(0), // NGINX_ATT_ID
				C.int(i),
				C.int(numWorkers),
				C.int(1), // stdout
			)
			if attachment != nil {
				w.mu.Lock()
				w.attachment = attachment
				w.mu.Unlock()
				w.ready.Store(true)
				log.Printf("nano attachment worker %d/%d registered", i+1, numWorkers)
				break
			}
			log.Printf("nano attachment worker %d/%d failed to register, retrying in 2s", i+1, numWorkers)
			time.Sleep(2 * time.Second)
		}
	}
}

// Ready reports whether every attachment instance finished registration.
func (m *AttachmentManager) Ready() bool {
	for _, w := range m.workers {
		if !w.ready.Load() {
			return false
		}
	}
	return true
}

// KeepAliveLoop periodically signals the nano agent that this attachment is alive.
func (m *AttachmentManager) KeepAliveLoop(interval time.Duration) {
	for {
		w := m.workers[0]
		if w.ready.Load() {
			w.mu.Lock()
			C.SendKeepAlive(w.attachment)
			w.mu.Unlock()
		}
		time.Sleep(interval)
	}
}

// GCLoop finalizes sessions that have been inactive for longer than the session TTL.
func (m *AttachmentManager) GCLoop(interval time.Duration) {
	for {
		time.Sleep(interval)
		deadline := time.Now().Add(-m.sessionTTL).UnixNano()
		var stale []*session
		m.sessionsMu.RLock()
		for _, s := range m.sessions {
			if s.lastActive.Load() < deadline {
				stale = append(stale, s)
			}
		}
		m.sessionsMu.RUnlock()
		for _, s := range stale {
			log.Printf("finalizing stale session %d", s.id)
			m.FiniSession(s.id)
		}
	}
}

// ReloadConfiguration asks every attachment instance to re-read its configuration.
func (m *AttachmentManager) ReloadConfiguration() error {
	var firstErr error
	for i, w := range m.workers {
		if !w.ready.Load() {
			continue
		}
		w.mu.Lock()
		res := C.RestartAttachmentConfiguration(w.attachment)
		w.mu.Unlock()
		if res != C.NANO_OK && firstErr == nil {
			firstErr = fmt.Errorf("worker %d failed to reload configuration", i)
		}
	}
	return firstErr
}

func (m *AttachmentManager) getSession(id uint32) *session {
	m.sessionsMu.RLock()
	defer m.sessionsMu.RUnlock()
	return m.sessions[id]
}

func (m *AttachmentManager) removeSession(id uint32) {
	m.sessionsMu.Lock()
	delete(m.sessions, id)
	m.sessionsMu.Unlock()
}

// FiniSession releases the session resources. It is safe to call for a session
// that has already been finalized.
func (m *AttachmentManager) FiniSession(id uint32) {
	s := m.getSession(id)
	if s == nil {
		return
	}
	m.removeSession(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data != nil {
		s.worker.mu.Lock()
		C.FiniSessionData(s.worker.attachment, s.data)
		s.worker.mu.Unlock()
		s.data = nil
	}
}

// StartTransaction creates a new session and sends the request metadata and
// headers for inspection. On a final verdict the session is finalized before
// returning.
func (m *AttachmentManager) StartTransaction(data *StartTransactionData) (uint32, *InspectionResult) {
	// Round-robin worker selection.
	sid := m.nextSessionID.Add(1)
	if sid == 0 { // CORRUPTED_SESSION_ID
		sid = m.nextSessionID.Add(1)
	}
	w := m.workers[int(sid)%len(m.workers)]
	if !w.ready.Load() {
		return 0, &InspectionResult{Verdict: VerdictNoop}
	}

	w.mu.Lock()
	sessionData := C.InitSessionData(w.attachment, C.SessionID(sid))
	w.mu.Unlock()
	if sessionData == nil {
		return 0, &InspectionResult{Verdict: VerdictNoop}
	}

	s := &session{id: sid, worker: w, data: sessionData}
	s.touch()
	m.sessionsMu.Lock()
	m.sessions[sid] = s
	m.sessionsMu.Unlock()

	m.stats.TransactionsStarted.Add(1)

	res := m.sendStartTransaction(s, data)
	if res.Verdict != VerdictInspect {
		m.FiniSession(sid)
		return 0, res
	}
	m.stats.TransactionsInspected.Add(1)
	return sid, res
}

// SendRequestBody sends a request body chunk for inspection.
func (m *AttachmentManager) SendRequestBody(id uint32, body []byte) *InspectionResult {
	return m.sendBody(id, body, C.HTTP_REQUEST_BODY)
}

// SendResponseBody sends a response body chunk for inspection. The result may
// carry a modified body when the agent requested an injection.
func (m *AttachmentManager) SendResponseBody(id uint32, body []byte) *InspectionResult {
	return m.sendBody(id, body, C.HTTP_RESPONSE_BODY)
}

// EndRequest signals that the request part of the session is fully sent.
func (m *AttachmentManager) EndRequest(id uint32) *InspectionResult {
	return m.sendEnd(id, C.HTTP_REQUEST_END)
}

// EndResponse signals that the response part of the session is fully sent.
func (m *AttachmentManager) EndResponse(id uint32) *InspectionResult {
	return m.sendEnd(id, C.HTTP_RESPONSE_END)
}

func nanoStr(s string) C.nano_str_t {
	// C.CString copies into C memory; required because cgo forbids passing Go
	// pointers embedded inside C structs.
	return C.nano_str_t{
		len:  C.size_t(len(s)),
		data: (*C.uchar)(unsafe.Pointer(C.CString(s))),
	}
}

func freeNanoStr(s *C.nano_str_t) {
	if s.data != nil {
		C.free(unsafe.Pointer(s.data))
		s.data = nil
	}
}

func buildHeaders(headers [][2]string) *C.HttpHeaders {
	count := len(headers)
	arr := C.createHttpHeaderDataArray(C.size_t(count))
	for i, kv := range headers {
		C.setHeaderElement(arr, C.size_t(i), nanoStr(kv[0]), nanoStr(kv[1]))
	}
	httpHeaders := (*C.HttpHeaders)(C.calloc(1, C.sizeof_HttpHeaders))
	httpHeaders.data = arr
	httpHeaders.headers_count = C.size_t(count)
	return httpHeaders
}

func freeHeaders(h *C.HttpHeaders) {
	if h == nil {
		return
	}
	if h.data != nil {
		headers := unsafe.Slice(h.data, int(h.headers_count))
		for i := range headers {
			freeNanoStr(&headers[i].key)
			freeNanoStr(&headers[i].value)
		}
		C.free(unsafe.Pointer(h.data))
	}
	C.free(unsafe.Pointer(h))
}

func (m *AttachmentManager) sendStartTransaction(s *session, data *StartTransactionData) *InspectionResult {
	metaData := (*C.HttpMetaData)(C.calloc(1, C.sizeof_HttpMetaData))
	metaData.http_protocol = nanoStr(data.Protocol)
	metaData.method_name = nanoStr(data.Method)
	metaData.host = nanoStr(data.Host)
	metaData.listening_ip = nanoStr(data.ListeningIP)
	metaData.listening_port = C.uint16_t(data.ListeningPort)
	metaData.uri = nanoStr(data.URI)
	metaData.client_ip = nanoStr(data.ClientIP)
	metaData.client_port = C.uint16_t(data.ClientPort)

	reqHeaders := buildHeaders(data.Headers)

	startData := (*C.HttpRequestFilterData)(C.calloc(1, C.sizeof_HttpRequestFilterData))
	startData.meta_data = metaData
	startData.req_headers = reqHeaders
	startData.contains_body = C.bool(data.ContainsBody)

	defer func() {
		freeNanoStr(&metaData.http_protocol)
		freeNanoStr(&metaData.method_name)
		freeNanoStr(&metaData.host)
		freeNanoStr(&metaData.listening_ip)
		freeNanoStr(&metaData.uri)
		freeNanoStr(&metaData.client_ip)
		C.free(unsafe.Pointer(metaData))
		freeHeaders(reqHeaders)
		C.free(unsafe.Pointer(startData))
	}()

	return m.sendChunk(s, C.HTTP_REQUEST_FILTER, C.DataBuffer(unsafe.Pointer(startData)), 0)
}

// SendResponseHeaders sends the upstream response headers for inspection.
func (m *AttachmentManager) SendResponseHeaders(id uint32, data *ResponseHeadersData) *InspectionResult {
	s := m.getSession(id)
	if s == nil {
		return &InspectionResult{Verdict: VerdictNoop}
	}
	s.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return &InspectionResult{Verdict: VerdictNoop}
	}

	headers := buildHeaders(data.Headers)
	resHeaders := (*C.ResHttpHeaders)(C.calloc(1, C.sizeof_ResHttpHeaders))
	resHeaders.headers = headers
	resHeaders.response_code = C.uint16_t(data.Code)
	resHeaders.content_length = C.uint64_t(data.ContentLength)
	defer func() {
		freeHeaders(headers)
		C.free(unsafe.Pointer(resHeaders))
	}()

	res := m.sendChunk(s, C.HTTP_RESPONSE_HEADER, C.DataBuffer(unsafe.Pointer(resHeaders)), 0)
	if res.Verdict != VerdictInspect {
		m.finalizeLocked(s)
	}
	return res
}

func (m *AttachmentManager) sendBody(id uint32, body []byte, chunkType C.HttpChunkType) *InspectionResult {
	s := m.getSession(id)
	if s == nil {
		return &InspectionResult{Verdict: VerdictNoop}
	}
	s.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return &InspectionResult{Verdict: VerdictNoop}
	}
	if len(body) == 0 {
		return &InspectionResult{Verdict: VerdictInspect}
	}

	// Copy the body into C memory once; the nano_str chunks point into it.
	cBody := C.CBytes(body)
	defer C.free(cBody)

	numChunks := ((len(body) - 1) / bodyChunkSize) + 1
	chunks := C.createNanoStrArray(C.size_t(numChunks))
	defer C.free(unsafe.Pointer(chunks))

	for i := 0; i < numChunks; i++ {
		offset := i * bodyChunkSize
		size := len(body) - offset
		if size > bodyChunkSize {
			size = bodyChunkSize
		}
		C.setNanoStrElement(chunks, C.size_t(i), C.nano_str_t{
			len:  C.size_t(size),
			data: (*C.uchar)(unsafe.Pointer(uintptr(cBody) + uintptr(offset))),
		})
	}

	httpBody := (*C.NanoHttpBody)(C.calloc(1, C.sizeof_NanoHttpBody))
	httpBody.data = chunks
	httpBody.bodies_count = C.size_t(numChunks)
	defer C.free(unsafe.Pointer(httpBody))

	res := m.sendChunkWithBody(s, chunkType, C.DataBuffer(unsafe.Pointer(httpBody)), body, numChunks)
	if res.Verdict != VerdictInspect {
		m.finalizeLocked(s)
	}
	return res
}

func (m *AttachmentManager) sendEnd(id uint32, chunkType C.HttpChunkType) *InspectionResult {
	s := m.getSession(id)
	if s == nil {
		return &InspectionResult{Verdict: VerdictNoop}
	}
	s.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return &InspectionResult{Verdict: VerdictNoop}
	}
	res := m.sendChunk(s, chunkType, nil, 0)
	if res.Verdict != VerdictInspect || chunkType == C.HTTP_RESPONSE_END {
		m.finalizeLocked(s)
	}
	return res
}

// finalizeLocked releases the session's C resources. The session mutex must be held.
func (m *AttachmentManager) finalizeLocked(s *session) {
	m.removeSession(s.id)
	if s.data != nil {
		s.worker.mu.Lock()
		C.FiniSessionData(s.worker.attachment, s.data)
		s.worker.mu.Unlock()
		s.data = nil
	}
}

func (m *AttachmentManager) sendChunk(
	s *session,
	chunkType C.HttpChunkType,
	buffer C.DataBuffer,
	numChunks int,
) *InspectionResult {
	return m.sendChunkWithBody(s, chunkType, buffer, nil, numChunks)
}

func (m *AttachmentManager) sendChunkWithBody(
	s *session,
	chunkType C.HttpChunkType,
	buffer C.DataBuffer,
	body []byte,
	numChunks int,
) *InspectionResult {
	m.stats.ChunksSent.Add(1)

	attachmentData := (*C.AttachmentData)(C.calloc(1, C.sizeof_AttachmentData))
	attachmentData.session_id = C.SessionID(s.id)
	attachmentData.chunk_type = chunkType
	attachmentData.session_data = s.data
	attachmentData.data = buffer
	defer C.free(unsafe.Pointer(attachmentData))

	s.worker.mu.Lock()
	response := C.SendDataNanoAttachment(s.worker.attachment, attachmentData)
	s.worker.mu.Unlock()

	result := m.buildResult(s, &response, body, numChunks)

	s.worker.mu.Lock()
	C.FreeAttachmentResponseContent(s.worker.attachment, s.data, &response)
	s.worker.mu.Unlock()

	return result
}

func (m *AttachmentManager) buildResult(
	s *session,
	response *C.AttachmentVerdictResponse,
	body []byte,
	numChunks int,
) *InspectionResult {
	switch response.verdict {
	case C.ATTACHMENT_VERDICT_ACCEPT:
		return &InspectionResult{Verdict: VerdictAccept}
	case C.ATTACHMENT_VERDICT_DROP:
		return &InspectionResult{Verdict: VerdictDrop, Block: m.buildBlockResponse(s, response)}
	case C.ATTACHMENT_VERDICT_INJECT:
		result := &InspectionResult{Verdict: VerdictInspect}
		if body != nil && response.modifications != nil {
			result.ModifiedBody = applyBodyModifications(body, response.modifications, numChunks)
			result.BodyModified = true
		}
		return result
	default:
		// INSPECT and DELAYED (async only) both mean "keep going".
		result := &InspectionResult{Verdict: VerdictInspect}
		if body != nil && response.modifications != nil {
			result.ModifiedBody = applyBodyModifications(body, response.modifications, numChunks)
			result.BodyModified = true
		}
		return result
	}
}

// applyBodyModifications applies injection modifications to the body that was
// split into numChunks buffers of bodyChunkSize bytes.
func applyBodyModifications(body []byte, modifications *C.NanoHttpModificationList, numChunks int) []byte {
	// Re-assemble per-chunk injections. orig_buff_index refers to the index of
	// the 8KB chunk the injection position is relative to.
	type injection struct {
		pos  int
		data string
	}
	var injections []injection
	for mod := modifications; mod != nil; mod = mod.next {
		if mod.modification.is_header != 0 {
			continue
		}
		chunkIndex := int(mod.modification.orig_buff_index)
		if chunkIndex >= numChunks {
			continue
		}
		pos := chunkIndex*bodyChunkSize + int(mod.modification.injection_pos)
		if pos < 0 || pos > len(body) {
			continue
		}
		injections = append(injections, injection{pos: pos, data: C.GoString(mod.modification_buffer)})
	}
	if len(injections) == 0 {
		return body
	}
	result := make([]byte, 0, len(body))
	last := 0
	for _, inj := range injections {
		if inj.pos < last {
			continue
		}
		result = append(result, body[last:inj.pos]...)
		result = append(result, inj.data...)
		last = inj.pos
	}
	result = append(result, body[last:]...)
	return result
}

func nanoStrToBytes(s C.nano_str_t) []byte {
	if s.data == nil || s.len == 0 {
		return nil
	}
	return C.GoBytes(unsafe.Pointer(s.data), C.int(s.len))
}

func (m *AttachmentManager) buildBlockResponse(s *session, response *C.AttachmentVerdictResponse) *BlockResponse {
	if response.web_response_data == nil {
		return &BlockResponse{Code: 403}
	}

	s.worker.mu.Lock()
	defer s.worker.mu.Unlock()

	responseType := C.GetWebResponseType(s.worker.attachment, s.data, response)
	switch responseType {
	case C.RESPONSE_CODE_ONLY:
		return &BlockResponse{Code: int(C.GetResponseCode(response))}

	case C.REDIRECT_WEB_RESPONSE:
		redirect := C.GetRedirectPage(s.worker.attachment, s.data, response)
		location := string(nanoStrToBytes(redirect.redirect_location))
		return &BlockResponse{
			Code:    307,
			Headers: map[string]string{"Location": location},
		}

	case C.CUSTOM_RESPONSE_WITH_HEADERS:
		custom := C.GetCustomResponseWithHeaders(s.worker.attachment, s.data, response)
		if custom == nil {
			return &BlockResponse{Code: 403}
		}
		block := &BlockResponse{
			Code:    int(custom.response_code),
			Headers: make(map[string]string),
		}
		if custom.headers_count > 0 && custom.headers != nil {
			headers := unsafe.Slice(custom.headers, int(custom.headers_count))
			for _, h := range headers {
				key := C.GoStringN(h.key, C.int(h.key_size))
				value := C.GoStringN(h.value, C.int(h.value_size))
				block.Headers[key] = value
			}
		}
		if custom.body_size > 0 && custom.body != nil {
			block.Body = C.GoStringN(custom.body, C.int(custom.body_size))
		}
		return block

	default: // CUSTOM_WEB_RESPONSE / CUSTOM_WEB_BLOCK_PAGE_RESPONSE
		blockPage := C.GetBlockPage(s.worker.attachment, s.data, response)
		var buf []byte
		buf = append(buf, nanoStrToBytes(blockPage.title_prefix)...)
		buf = append(buf, nanoStrToBytes(blockPage.title)...)
		buf = append(buf, nanoStrToBytes(blockPage.body_prefix)...)
		buf = append(buf, nanoStrToBytes(blockPage.body)...)
		buf = append(buf, nanoStrToBytes(blockPage.uuid_prefix)...)
		buf = append(buf, nanoStrToBytes(blockPage.uuid)...)
		buf = append(buf, nanoStrToBytes(blockPage.uuid_suffix)...)
		return &BlockResponse{
			Code:    int(blockPage.response_code),
			Headers: map[string]string{"Content-Type": "text/html"},
			Body:    string(buf),
		}
	}
}
