package viewer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xeroxgo/kiosk-daemon/internal/audio"
)

// Config specifies configuration parameters for the live preview viewer HTTP server.
type Config struct {
	Addr        string        // e.g. ":8081" or "0.0.0.0:8081"
	PreviewPNG  string        // Exact path to preview PNG file (e.g. /tmp/xeroxgo-viewer/screen.png)
	SoundPlayer audio.Player  // Sound player providing chime WAV
	KioskID     string        // Identifier for display
	PollRate    time.Duration // Client-side polling rate recommended (e.g. 200ms)
}

// Server provides a secure, minimal HTTP server displaying the live rendered kiosk screen.
// It serves ONLY its embedded viewer HTML, the exact preview PNG, and the completion chime.
// It explicitly never exposes or traverses arbitrary filesystem directories.
type Server struct {
	cfg        Config
	httpServer *http.Server
	listener   net.Listener
	closed     atomic.Bool
	mu         sync.RWMutex

	lastCompletedJob  string
	lastCompletedTime int64
}

// NotifyJobCompleted records that jobID has been confirmed completed.
// Connected browser monitors poll this authoritative signal to play completion audio once.
func (s *Server) NotifyJobCompleted(jobID string) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCompletedJob = jobID
	s.lastCompletedTime = time.Now().UTC().UnixMilli()
}

// NewServer creates a new viewer HTTP server.
func NewServer(cfg Config) (*Server, error) {
	if cfg.PreviewPNG == "" {
		return nil, fmt.Errorf("viewer: PreviewPNG path is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8081"
	}
	if cfg.SoundPlayer == nil {
		cfg.SoundPlayer = audio.NoopPlayer{}
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("viewer: failed to listen on %s: %w", cfg.Addr, err)
	}

	s := &Server{
		cfg:      cfg,
		listener: ln,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/screen.png", s.handleScreenPNG)
	mux.HandleFunc("/chime.wav", s.handleChimeWAV)
	mux.HandleFunc("/status", s.handleStatus)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	return s, nil
}

// Addr returns the bound network address of the viewer server.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.cfg.Addr
}

// Start runs the HTTP server until the provided context is canceled.
func (s *Server) Start(ctx context.Context) {
	log.Printf("[VIEWER] Live kiosk monitor available at http://%s (viewing %s)", s.Addr(), filepath.Clean(s.cfg.PreviewPNG))

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	if err := s.httpServer.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		log.Printf("[VIEWER] HTTP server error: %v", err)
	}
}

// Close stops the HTTP server.
func (s *Server) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// handleIndex serves the self-contained live monitor web page.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write([]byte(viewerHTML))
}

// handleScreenPNG streams the current preview PNG file.
// It strictly serves only cfg.PreviewPNG, disallowing any directory traversal.
func (s *Server) handleScreenPNG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	data, err := os.ReadFile(s.cfg.PreviewPNG)
	if err != nil {
		// If preview PNG is not yet rendered by the daemon, return a 1x1 transparent placeholder
		w.Header().Set("X-XeroxGo-Status", "waiting_for_frame")
		_, _ = w.Write(transparent1x1PNG)
		return
	}

	w.Header().Set("X-XeroxGo-Status", "live")
	_, _ = w.Write(data)
}

// handleChimeWAV serves the completion audio file for browser playback.
func (s *Server) handleChimeWAV(w http.ResponseWriter, r *http.Request) {
	wavData := s.cfg.SoundPlayer.WAVBytes()
	if len(wavData) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(wavData)
}

// handleStatus returns kiosk monitor metadata in JSON.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	fi, err := os.Stat(s.cfg.PreviewPNG)
	var modTime time.Time
	var size int64
	ready := err == nil
	if ready {
		modTime = fi.ModTime().UTC()
		size = fi.Size()
	}

	s.mu.RLock()
	lastJob := s.lastCompletedJob
	lastTime := s.lastCompletedTime
	s.mu.RUnlock()

	resp := map[string]interface{}{
		"kiosk_id":              s.cfg.KioskID,
		"status":                "ok",
		"preview_ready":         ready,
		"last_render":           modTime,
		"file_size":             size,
		"last_completed_job_id": lastJob,
		"last_completed_at":     lastTime,
		"current_time":          time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(resp)
}

// Minimal 1x1 transparent PNG fallback before the first frame is rendered
var transparent1x1PNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

// viewerHTML is the self-contained, responsive live kiosk screen monitor page.
// It renders the real kiosk image stream full-screen without extraneous chrome or debug overlays.
const viewerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
  <title>XeroxGo Kiosk Live Monitor</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    html, body {
      background-color: #000000;
      width: 100vw;
      height: 100vh;
      overflow: hidden;
      display: flex;
      align-items: center;
      justify-content: center;
    }
    #screen {
      width: 100vw;
      height: 100vh;
      object-fit: contain;
      display: block;
    }
  </style>
</head>
<body>
  <img id="screen" src="/screen.png" alt="XeroxGo Live Kiosk Screen">
  <audio id="chime" src="/chime.wav" preload="auto"></audio>

  <script>
    let isFetching = false;
    const playedCompletedJobs = new Set();
    const img = document.getElementById('screen');
    const chime = document.getElementById('chime');

    // Unlock browser audio seamlessly on first click anywhere on screen
    window.addEventListener('click', function() {
      chime.play().then(() => {
        chime.pause();
        chime.currentTime = 0;
      }).catch(() => {});
    }, { once: true });

    // High performance buffer refresh loop without flicker
    function updateFrame() {
      if (isFetching) return;
      isFetching = true;
      const nextImg = new Image();
      const targetSrc = '/screen.png?t=' + Date.now();

      nextImg.onload = function() {
        img.src = targetSrc;
        isFetching = false;
      };

      nextImg.onerror = function() {
        isFetching = false;
      };

      nextImg.src = targetSrc;
    }

    // Authoritative completion polling: plays chime once per completed job
    function checkStatus() {
      fetch('/status')
        .then(function(res) { return res.json(); })
        .then(function(data) {
          if (data && data.last_completed_job_id) {
            const jobID = data.last_completed_job_id;
            if (!playedCompletedJobs.has(jobID)) {
              playedCompletedJobs.add(jobID);
              chime.currentTime = 0;
              chime.play().catch(function() {});
            }
          }
        })
        .catch(function() {});
    }

    setInterval(updateFrame, 200);
    setInterval(checkStatus, 300);
  </script>
</body>
</html>
`

