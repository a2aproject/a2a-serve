package adksidecar

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

const defaultMaxSessions = 256

// SessionWorkspace serves an ADK session-scoped in-memory workspace over the
// a2a-serve-sidecar HTTP workspace protocol and syncs files to ADK artifacts.
type SessionWorkspace struct {
	seedDir     string
	maxSessions int

	mu       sync.Mutex
	trees    map[string]*list.Element
	lru      *list.List
	seedTree fsys.FS
}

type sessionEntry struct {
	id   string
	tree fsys.FS
}

// NewSessionWorkspace returns a workspace seeded from seedDir (optional).
// maxSessions defaults to 256 when <= 0.
func NewSessionWorkspace(seedDir string, maxSessions int) *SessionWorkspace {
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	return &SessionWorkspace{
		seedDir:     seedDir,
		maxSessions: maxSessions,
		trees:       make(map[string]*list.Element),
		lru:         list.New(),
	}
}

// TreeFor returns the session's workspace tree, seeding it on first use.
func (w *SessionWorkspace) TreeFor(sessionID string) fsys.FS {
	w.mu.Lock()
	defer w.mu.Unlock()
	if elem, ok := w.trees[sessionID]; ok {
		w.lru.MoveToFront(elem)
		return elem.Value.(*sessionEntry).tree
	}
	tree := newSeededMemory(w.seedDir)
	elem := w.lru.PushFront(&sessionEntry{id: sessionID, tree: tree})
	w.trees[sessionID] = elem
	for w.lru.Len() > w.maxSessions {
		oldest := w.lru.Back()
		if oldest == nil {
			break
		}
		entry := w.lru.Remove(oldest).(*sessionEntry)
		delete(w.trees, entry.id)
	}
	return tree
}

// SeedTree returns the read-only seed tree seen by requests without a session.
func (w *SessionWorkspace) SeedTree() fsys.FS {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seedTree == nil {
		w.seedTree = newSeededMemory(w.seedDir)
	}
	return w.seedTree
}

// Forget drops the workspace tree of an ended ADK session.
func (w *SessionWorkspace) Forget(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if elem, ok := w.trees[sessionID]; ok {
		w.lru.Remove(elem)
		delete(w.trees, sessionID)
	}
}

// SyncToArtifacts saves all regular files in the session's workspace as ADK
// session artifacts under "workspace:<path>" (replacing '/' with ':' because
// ADK's artifact service rejects path separators in filenames).
func (w *SessionWorkspace) SyncToArtifacts(ctx agent.InvocationContext) ([]string, error) {
	artifacts := ctx.Artifacts()
	sess := ctx.Session()
	if artifacts == nil || sess == nil || sess.ID() == "" {
		return nil, nil
	}
	tree := w.TreeFor(sess.ID())
	files, err := snapshotFiles(ctx, tree)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		part := genai.NewPartFromBytes(files[p], "application/octet-stream")
		artifactName := "workspace:" + strings.ReplaceAll(p, "/", ":")
		if _, err := artifacts.Save(ctx, artifactName, part); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// Handler returns an http.Handler serving the workspace HTTP protocol (paths
// relative to the mount point: /stat, /list, /file, /mkdir, /symlink, /rename,
// /changes).
func (w *SessionWorkspace) Handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctxID := r.Header.Get(HeaderContext)
		isWrite := r.Method != http.MethodGet && r.Method != http.MethodHead
		var tree fsys.FS
		if ctxID != "" {
			tree = w.TreeFor(ctxID)
		} else {
			if isWrite {
				writeWorkspaceError(rw, http.StatusForbidden, "permission",
					"a change without A2A-Sidecar-Context has no session; SessionWorkspace needs --workspace-sync turn")
				return
			}
			tree = w.SeedTree()
		}
		fsys.Handler(tree).ServeHTTP(rw, r)
	})
}

func writeWorkspaceError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
		"code":  code,
	}); err != nil {
		slog.Debug("writing workspace error response", "error", err)
	}
}

func newSeededMemory(seedDir string) fsys.FS {
	mem := fsys.Memory()
	if seedDir == "" {
		return mem
	}
	absSeed, err := filepath.Abs(seedDir)
	if err != nil {
		slog.Warn("resolving workspace seed directory", "dir", seedDir, "error", err)
		return mem
	}
	info, err := os.Stat(absSeed)
	if err != nil || !info.IsDir() {
		return mem
	}
	ctx := context.Background()
	err = filepath.WalkDir(absSeed, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == absSeed {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "__pycache__" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(absSeed, p)
		if err != nil {
			return err
		}
		wirePath := filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return mem.Symlink(ctx, target, wirePath)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		perm := fi.Mode().Perm()
		if d.IsDir() {
			if perm == 0 {
				perm = 0o755
			}
			return mem.Mkdir(ctx, wirePath, perm)
		}
		if perm == 0 {
			perm = 0o644
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = mem.Write(ctx, wirePath, bytes.NewReader(data), perm, "")
		return err
	})
	if err != nil {
		slog.Warn("seeding workspace memory tree", "dir", seedDir, "error", err)
	}
	return mem
}

func snapshotFiles(ctx context.Context, tree fsys.FS) (map[string][]byte, error) {
	out := make(map[string][]byte)
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := tree.ReadDir(ctx, dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Mode.IsDir() {
				if err := walk(e.Path); err != nil {
					return err
				}
				continue
			}
			if !e.Mode.IsRegular() {
				continue
			}
			rc, err := tree.Open(ctx, e.Path)
			if err != nil {
				return err
			}
			data, readErr := io.ReadAll(rc)
			closeErr := rc.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			out[path.Clean(e.Path)] = data
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return out, nil
}
