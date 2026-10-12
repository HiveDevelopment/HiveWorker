package api

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"hivepanel-worker/internal/allocation"
)

type nectarTCPRequest struct {
	ReadJSON  bool   `json:"read_json"`
	Action    string `json:"action"`
	Scope     string `json:"scope"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	SessionID string `json:"session_id"`
	Data      string `json:"data_base64"`
	ReadLine  bool   `json:"read_line"`
}

type nectarTCPSession struct {
	mutex      sync.Mutex
	conn       net.Conn
	reader     *bufio.Reader
	cellID     string
	scope      string
	ip         string
	port       int
	expires    time.Time
	operations int
}

type nectarTCPPool struct {
	mutex    sync.Mutex
	sessions map[string]*nectarTCPSession
	opening  int
}

func allocationTarget(ip string, port int, assigned []allocation.Allocation) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid allocation")
	}
	address := net.ParseIP(ip)
	if address == nil || address.IsMulticast() {
		return "", errors.New("allocation must contain an IP address")
	}
	allowed := false
	for _, item := range assigned {
		other := net.ParseIP(item.IP)
		if other != nil && other.Equal(address) && item.Port == port {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errors.New("allocation is not assigned to this cell")
	}
	if address.IsUnspecified() {
		if address.To4() != nil {
			ip = "127.0.0.1"
		} else {
			ip = "::1"
		}
	}
	return net.JoinHostPort(ip, fmt.Sprint(port)), nil
}

func (pool *nectarTCPPool) remove(id string, session *nectarTCPSession) {
	pool.mutex.Lock()
	if pool.sessions[id] == session {
		delete(pool.sessions, id)
	}
	pool.mutex.Unlock()
	_ = session.conn.Close()
}

func (h *Handler) NectarTCP(w http.ResponseWriter, r *http.Request) {
	var input nectarTCPRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 24576)).Decode(&input) != nil || len(input.Scope) != 64 {
		http.Error(w, "invalid request", 400)
		return
	}
	gameCell, exists := h.Manager.Get(r.PathValue("id"))
	if !exists {
		http.Error(w, "cell not found", 404)
		return
	}
	assigned := append([]allocation.Allocation{gameCell.Allocation}, gameCell.AdditionalAllocations...)
	target, err := allocationTarget(input.IP, input.Port, assigned)
	if err != nil {
		http.Error(w, "allocation denied", 403)
		return
	}
	pool := &h.nectarTCP
	var session *nectarTCPSession
	id := input.SessionID
	if input.Action == "open" {
		pool.mutex.Lock()
		if pool.sessions == nil {
			pool.sessions = make(map[string]*nectarTCPSession)
		}
		if len(pool.sessions)+pool.opening >= 32 {
			pool.mutex.Unlock()
			http.Error(w, "TCP session limit reached", 429)
			return
		}
		pool.opening++
		pool.mutex.Unlock()
		conn, dialError := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(r.Context(), "tcp", target)
		pool.mutex.Lock()
		pool.opening--
		if dialError != nil {
			pool.mutex.Unlock()
			http.Error(w, "TCP connection failed", 502)
			return
		}
		var token [32]byte
		if _, err = rand.Read(token[:]); err != nil {
			pool.mutex.Unlock()
			conn.Close()
			http.Error(w, "session creation failed", 500)
			return
		}
		id = hex.EncodeToString(token[:])
		session = &nectarTCPSession{conn: conn, reader: bufio.NewReaderSize(conn, 8192), cellID: gameCell.ID, scope: input.Scope, ip: input.IP, port: input.Port, expires: time.Now().Add(20 * time.Second)}
		pool.sessions[id] = session
		pool.mutex.Unlock()
		time.AfterFunc(20*time.Second, func() { pool.remove(id, session) })
	} else {
		pool.mutex.Lock()
		session = pool.sessions[id]
		pool.mutex.Unlock()
	}
	if session == nil || session.cellID != gameCell.ID || session.scope != input.Scope || session.ip != input.IP || session.port != input.Port {
		http.Error(w, "TCP session denied", 403)
		return
	}
	session.mutex.Lock()
	defer session.mutex.Unlock()
	if time.Now().After(session.expires) || session.operations >= 4 {
		pool.remove(id, session)
		http.Error(w, "TCP session expired", 410)
		return
	}
	session.operations++
	if input.Action == "close" {
		pool.remove(id, session)
		writeJSON(w, map[string]any{"closed": true})
		return
	}
	if input.Action != "open" && input.Action != "exchange" {
		pool.remove(id, session)
		http.Error(w, "invalid action", 400)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	if deadline.After(session.expires) {
		deadline = session.expires
	}
	_ = session.conn.SetDeadline(deadline)
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil || len(data) > 16384 {
		pool.remove(id, session)
		http.Error(w, "invalid data", 400)
		return
	}
	if len(data) > 0 {
		_, err = io.Copy(session.conn, bytes.NewReader(data))
	}
	var received []byte
	if err == nil && (input.ReadLine || input.ReadJSON) {
		for len(received) < 8192 {
			var value byte
			value, err = session.reader.ReadByte()
			if err != nil {
				break
			}
			received = append(received, value)
			if input.ReadLine && value == '\n' {
				break
			}
			if input.ReadJSON && json.Valid(received) {
				break
			}
		}
		if len(received) == 8192 && ((input.ReadLine && received[len(received)-1] != '\n') || (input.ReadJSON && !json.Valid(received))) {
			err = errors.New("response too large")
		}
	}
	if err != nil {
		pool.remove(id, session)
		http.Error(w, "TCP exchange failed", 502)
		return
	}
	writeJSON(w, map[string]any{"session_id": id, "data_base64": base64.StdEncoding.EncodeToString(received)})
}
