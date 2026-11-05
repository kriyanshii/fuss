package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// toykv is a tiny key-value server with a planted durability bug:
// PUT returns 200 before the write is on disk. The in-memory map is
// flushed to <data-dir>/store every 200ms.

func main() {
	dataDir := flag.String("data-dir", "", "directory for the store file")
	addr := flag.String("addr", "127.0.0.1:18731", "listen address")
	doFsync := flag.Bool("fsync", false, "sync the store to disk before PUT returns 200")
	flag.Parse()
	if *dataDir == "" {
		log.Fatal("-data-dir is required")
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	s := &store{path: filepath.Join(*dataDir, "store"), fsync: *doFsync, m: map[string]string{}}
	s.load()
	if !s.fsync {
		go s.flushLoop()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/k/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/k/")
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			v, ok := s.m[key]
			s.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, v)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.m[key] = string(body)
			if s.fsync {
				if err := s.writeLocked(); err != nil {
					s.mu.Unlock()
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type store struct {
	path  string
	fsync bool
	mu    sync.Mutex
	m     map[string]string
}

func (s *store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "\t")
		if ok && k != "" {
			s.m[k] = v
		}
	}
}

func (s *store) writeLocked() error {
	var b strings.Builder
	for k, v := range s.m {
		b.WriteString(k)
		b.WriteByte('\t')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, s.path)
}

func (s *store) flushLoop() {
	for {
		time.Sleep(200 * time.Millisecond)
		s.mu.Lock()
		_ = s.writeLocked()
		s.mu.Unlock()
	}
}
