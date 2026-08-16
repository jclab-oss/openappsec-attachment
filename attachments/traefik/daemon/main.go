// openappsec-traefik-daemon bridges the traefik openappsec plugin with the
// open-appsec nano agent. It links the nano attachment C library (shared
// memory IPC with the agent) and exposes a small HTTP API consumed by the
// pure-Go traefik middleware plugin.
package main

import (
	"log"
	"os"
	"runtime"
	"strconv"
	"time"
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func getConcurrency() int {
	method := getEnv("CONCURRENCY_CALC", "numOfCores")
	var raw string
	switch method {
	case "numOfCores":
		return runtime.NumCPU()
	case "istioCpuLimit":
		raw = getEnv("ISTIO_CPU_LIMIT", "-1")
	case "custom":
		raw = getEnv("CONCURRENCY_NUMBER", "-1")
	default:
		log.Printf("unknown concurrency method %q, using number of CPU cores", method)
		return runtime.NumCPU()
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("invalid concurrency number %q, using number of CPU cores", raw)
		return runtime.NumCPU()
	}
	return value
}

func main() {
	log.SetPrefix("[openappsec-traefik-daemon] ")

	listenAddr := getEnv("OPENAPPSEC_DAEMON_LISTEN", "127.0.0.1:8579")
	sessionTTLSec, err := strconv.Atoi(getEnv("OPENAPPSEC_SESSION_TTL_SEC", "120"))
	if err != nil || sessionTTLSec <= 0 {
		sessionTTLSec = 120
	}
	numWorkers := getConcurrency()
	log.Printf("starting with %d attachment workers", numWorkers)

	manager := NewAttachmentManager(numWorkers, time.Duration(sessionTTLSec)*time.Second)
	go manager.Init()
	go manager.KeepAliveLoop(30 * time.Second)
	go manager.GCLoop(10 * time.Second)

	server := NewServer(manager)
	if err := server.Serve(listenAddr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
