package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/vpatelsj/stargate/dcclient"
)

// MockJob represents an in-flight job in the mock DC
type MockJob struct {
	ID        string
	ServerID  string
	OSVersion string
	Phase     string
	Message   string
	StartTime time.Time
}

// MockDCServer simulates a datacenter API
type MockDCServer struct {
	mu   sync.RWMutex
	jobs map[string]*MockJob

	// Configuration
	repaveDuration time.Duration
	failureRate    float64 // 0.0 to 1.0
}

func NewMockDCServer() *MockDCServer {
	return &MockDCServer{
		jobs:           make(map[string]*MockJob),
		repaveDuration: 30 * time.Second, // Default: 30 seconds to complete a repave
		failureRate:    0.0,              // Default: no failures
	}
}

// generateJobID creates a simple job ID
func (s *MockDCServer) generateJobID() string {
	return fmt.Sprintf("job-%d", time.Now().UnixNano())
}

// handleRepave handles POST /repave
func (s *MockDCServer) handleRepave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req dcclient.RepaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("Received repave request: server=%s, mac=%s, osVersion=%s", req.ServerID, req.MAC, req.OSVersion)

	s.mu.Lock()
	jobID := s.generateJobID()
	job := &MockJob{
		ID:        jobID,
		ServerID:  req.ServerID,
		OSVersion: req.OSVersion,
		Phase:     "running",
		Message:   "Repave in progress",
		StartTime: time.Now(),
	}
	s.jobs[jobID] = job
	s.mu.Unlock()

	// Start async job processing
	go s.processJob(jobID)

	resp := dcclient.RepaveResponse{
		JobID:  jobID,
		Status: "accepted",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(resp)
}

// processJob simulates the repave process
func (s *MockDCServer) processJob(jobID string) {
	// Simulate repave taking some time
	stages := []struct {
		delay   time.Duration
		message string
	}{
		{5 * time.Second, "Initiating PXE boot"},
		{5 * time.Second, "Downloading OS image"},
		{10 * time.Second, "Installing OS"},
		{5 * time.Second, "Configuring system"},
		{5 * time.Second, "Finalizing"},
	}

	for _, stage := range stages {
		time.Sleep(stage.delay)

		s.mu.Lock()
		if job, ok := s.jobs[jobID]; ok {
			job.Message = stage.message
			log.Printf("Job %s: %s", jobID, stage.message)
		}
		s.mu.Unlock()
	}

	// Complete the job
	s.mu.Lock()
	if job, ok := s.jobs[jobID]; ok {
		// Simulate occasional failures based on failureRate
		// For PoC, we always succeed
		job.Phase = "succeeded"
		job.Message = "Repave completed successfully"
		log.Printf("Job %s: completed successfully", jobID)
	}
	s.mu.Unlock()
}

// handleJobStatus handles GET /jobs/{id}
func (s *MockDCServer) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract job ID from path: /jobs/{id}
	jobID := r.URL.Path[len("/jobs/"):]
	if jobID == "" {
		http.Error(w, "Job ID required", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	job, ok := s.jobs[jobID]
	s.mu.RUnlock()

	if !ok {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	resp := dcclient.JobStatus{
		JobID:   job.ID,
		Phase:   job.Phase,
		Message: job.Message,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleHealth handles GET /health
func (s *MockDCServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dcName := os.Getenv("DC_NAME")
	if dcName == "" {
		dcName = "mock-dc"
	}

	server := NewMockDCServer()

	http.HandleFunc("/health", server.handleHealth)
	http.HandleFunc("/repave", server.handleRepave)
	http.HandleFunc("/jobs/", server.handleJobStatus)

	log.Printf("Starting Mock DC API server (%s) on port %s", dcName, port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
