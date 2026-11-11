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
// It renders the real kiosk image stream and does NOT duplicate kiosk UI logic.
const viewerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
  <title>XeroxGo Kiosk Live Monitor</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    html, body {
      background-color: #0b0f19;
      color: #e2e8f0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      width: 100vw;
      height: 100vh;
      overflow: hidden;
      display: flex;
      flex-direction: column;
    }
    header {
      background: #111827;
      border-bottom: 1px solid #1f2937;
      padding: 8px 16px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      font-size: 13px;
      user-select: none;
      z-index: 10;
    }
    .badge {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      background: #064e3b;
      color: #34d399;
      padding: 3px 10px;
      border-radius: 9999px;
      font-weight: 600;
      font-size: 12px;
      letter-spacing: 0.05em;
    }
    .badge-dot {
      width: 8px;
      height: 8px;
      background-color: #10b981;
      border-radius: 50%;
      animation: pulse 2s cubic-bezier(0.4, 0, 0.6, 1) infinite;
    }
    @keyframes pulse {
      0%, 100% { opacity: 1; transform: scale(1); }
      50% { opacity: 0.4; transform: scale(0.85); }
    }
    .controls {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    .hint {
      color: #94a3b8;
      font-size: 12px;
    }
    .btn-audio {
      background: #1e293b;
      border: 1px solid #334155;
      color: #cbd5e1;
      padding: 4px 10px;
      border-radius: 6px;
      cursor: pointer;
      font-size: 12px;
      display: flex;
      align-items: center;
      gap: 5px;
    }
    .btn-audio:hover { background: #334155; color: #fff; }
    .stage {
      flex: 1;
      display: flex;
      align-items: center;
      justify-content: center;
      background: #000000;
      position: relative;
    }
    #screen {
      max-width: 100vw;
      max-height: calc(100vh - 42px);
      width: auto;
      height: auto;
      aspect-ratio: 16 / 9;
      object-fit: contain;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.7);
    }
    #ssh-hint-banner {
      position: absolute;
      bottom: 12px;
      background: rgba(15, 23, 42, 0.85);
      backdrop-filter: blur(4px);
      border: 1px solid rgba(51, 65, 85, 0.6);
      padding: 6px 16px;
      border-radius: 8px;
      font-size: 12px;
      color: #94a3b8;
      pointer-events: none;
    }
    #ssh-hint-banner kbd {
      background: #334155;
      color: #f8fafc;
      padding: 1px 6px;
      border-radius: 4px;
      font-family: monospace;
      font-size: 11px;
    }
  </style>
</head>
<body>
  <header>
    <div style="display:flex; align-items:center; gap:12px;">
      <span class="badge"><span class="badge-dot"></span> LIVE MONITOR</span>
      <span style="font-weight:600; color:#f1f5f9;">XeroxGo Headless Kiosk</span>
    </div>
    <div class="controls">
      <span id="latency" class="hint">Refreshing...</span>
      <button id="audio-toggle" class="btn-audio" onclick="toggleAudio()">🔇 Audio: Click to Enable</button>
    </div>
  </header>

  <main class="stage">
    <img id="screen" src="/screen.png" alt="XeroxGo Live Kiosk Screen">
    <div id="ssh-hint-banner">
      Control over SSH: Type <kbd>PIN</kbd> then <kbd>Enter</kbd> to confirm &bull; <kbd>c</kbd> to clear &bull; <kbd>q</kbd> to cancel
    </div>
  </main>

  <audio id="chime" src="/chime.wav" preload="auto"></audio>

  <script>
    let audioUnlocked = false;
    let isFetching = false;
    const playedCompletedJobs = new Set();
    const img = document.getElementById('screen');
    const chime = document.getElementById('chime');
    const latencyEl = document.getElementById('latency');
    const audioBtn = document.getElementById('audio-toggle');

    function toggleAudio() {
      audioUnlocked = !audioUnlocked;
      audioBtn.textContent = audioUnlocked ? '🔔 Audio: Enabled' : '🔇 Audio: Click to Enable';
      if (audioUnlocked) {
        // Unlock browser audio context via explicit user gesture
        chime.play().then(() => {
          chime.pause();
          chime.currentTime = 0;
        }).catch(() => {});
      }
    }

    // High performance buffer refresh loop without flicker
    function updateFrame() {
      if (isFetching) return;
      isFetching = true;
      const t0 = performance.now();
      const nextImg = new Image();
      const targetSrc = '/screen.png?t=' + Date.now();

      nextImg.onload = function() {
        img.src = targetSrc;
        const dt = Math.round(performance.now() - t0);
        latencyEl.textContent = 'Sync: ' + dt + 'ms';
        isFetching = false;
      };

      nextImg.onerror = function() {
        latencyEl.textContent = 'Reconnecting...';
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
              if (audioUnlocked) {
                chime.currentTime = 0;
                chime.play().catch(function(e) {
                  console.log('Audio playback prevented:', e);
                });
              }
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

