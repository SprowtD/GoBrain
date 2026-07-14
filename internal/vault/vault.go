package vault

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// WriteRequest is a single Markdown file to persist into (or, with Delete,
// remove from) the vault. All mutations go through the single writer.
type WriteRequest struct {
	RelPath string // e.g. "youtube/<job-id>-00.md"
	Content string
	Delete  bool       // remove RelPath instead of writing Content
	Done    chan error // optional: writer sends the persist result when non-nil
}

var (
	writeCh     chan WriteRequest
	rootPath    string
	once        sync.Once
	remoteURL   string
	gitEnv      []string // extra env (GIT_SSH_COMMAND) for every git invocation
	authorName  = "secondbrain"
	authorEmail = "secondbrain@localhost"
	onCommit    func() // optional post-commit hook (e.g. re-embed for search)
)

// SetOnCommit registers a callback fired after each successful vault commit.
// Used to trigger semantic re-indexing without vault importing that package.
// The callback runs on its own goroutine so it never blocks the writer.
func SetOnCommit(fn func()) { onCommit = fn }

// Start boots the single-writer goroutine. ALL disk + git mutations happen on
// this one goroutine, so there are never concurrent writers to the vault.
func Start(path string) {
	once.Do(func() {
		rootPath = path
		remoteURL = os.Getenv("VAULT_REPO_URL")
		if n := os.Getenv("GIT_AUTHOR_NAME"); n != "" {
			authorName = n
		}
		if e := os.Getenv("GIT_AUTHOR_EMAIL"); e != "" {
			authorEmail = e
		}

		writeCh = make(chan WriteRequest, 256)
		if err := os.MkdirAll(path, 0o755); err != nil {
			log.Fatalf("vault: cannot create root %s: %v", path, err)
		}
		if err := setupSSH(); err != nil {
			log.Printf("vault: ssh key setup failed: %v", err)
		}
		if err := ensureRepo(); err != nil {
			log.Printf("vault: git repo setup failed: %v", err)
		}
		// Crash recovery: if a previous process died mid-debounce, the working
		// tree may hold uncommitted files. Commit them before accepting writes.
		if err := commit(); err != nil {
			log.Printf("vault: recovery commit failed: %v", err)
		}

		go writer()
	})
}

// Write enqueues a file for the single writer. Safe to call from any worker.
func Write(req WriteRequest) {
	writeCh <- req
}

// WriteSync enqueues a file and blocks until it's persisted to disk (the git
// commit still happens asynchronously on the debounce). Use for request/response
// writes that need the file to exist before returning.
func WriteSync(req WriteRequest) error {
	done := make(chan error, 1)
	req.Done = done
	writeCh <- req
	return <-done
}

// DeleteNote removes a note by vault-relative path, routed through the single
// writer so it's serialized with writes and picked up by the next commit (whose
// reconcile then prunes the note's embedding). Returns ErrNotFound if absent.
func DeleteNote(rel string) error {
	full, err := safeJoin(rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(full); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	done := make(chan error, 1)
	writeCh <- WriteRequest{RelPath: rel, Delete: true, Done: done}
	return <-done
}

func writer() {
	// Debounce git commits: keep persisting files as they arrive, and only
	// commit once writes go quiet for `debounce`.
	const debounce = 3 * time.Second
	timer := time.NewTimer(debounce)
	timer.Stop()
	dirty := false

	for {
		select {
		case req := <-writeCh:
			err := persist(req)
			if req.Done != nil {
				req.Done <- err
			}
			if err != nil {
				log.Printf("vault: write %s failed: %v", req.RelPath, err)
				continue
			}
			dirty = true
			timer.Reset(debounce)
		case <-timer.C:
			if dirty {
				if err := commit(); err != nil {
					log.Printf("vault: commit failed: %v", err)
				} else if onCommit != nil {
					go onCommit()
				}
				dirty = false
			}
		}
	}
}

func persist(req WriteRequest) error {
	full := filepath.Join(rootPath, req.RelPath)
	if req.Delete {
		// Already-gone is success (idempotent delete).
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// Write to a temp file then rename, so a concurrent reader never sees a
	// half-written note (reads don't go through the single writer).
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, []byte(req.Content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// commit stages everything, commits if there's anything new, and pushes to the
// configured remote (best-effort — a push failure is logged, not fatal, so the
// local vault stays the source of truth).
func commit() error {
	// Refresh the derived graph before staging: tag hub pages first (they're
	// new files the index should list), then index.md for every directory.
	// Identical output produces no git diff, so this is cheap.
	if err := writeTagPages(); err != nil {
		log.Printf("vault: tag page generation failed: %v", err)
	}
	if err := writeIndexes(); err != nil {
		log.Printf("vault: index generation failed: %v", err)
	}
	if err := runGit("add", "-A"); err != nil {
		return err
	}
	if !hasStagedChanges() {
		return nil // nothing to commit
	}
	if err := runGit(
		"-c", "user.name="+authorName,
		"-c", "user.email="+authorEmail,
		"commit", "-m", "vault: sync",
	); err != nil {
		return err
	}
	if remoteURL != "" {
		push()
	}
	return nil
}

// push reconciles with the remote before pushing so a teammate's direct edit to
// the vault repo doesn't cause a non-fast-forward rejection. All best-effort: a
// push failure leaves the commit local (the local vault stays source of truth).
func push() {
	// Rebase our new commit(s) on top of whatever the remote has. `fetch` of a
	// not-yet-existing remote branch errors on a fresh repo — in that case we
	// skip the rebase and let the push create the branch.
	if err := runGit("fetch", "origin", "main"); err == nil {
		if err := runGit("rebase", "origin/main"); err != nil {
			// A conflict would leave the tree mid-rebase; abort to keep the
			// working copy clean and let the next commit try again.
			log.Printf("vault: rebase on origin/main failed, aborting: %v", err)
			_ = runGit("rebase", "--abort")
		}
	}
	if err := runGit("push", "origin", "HEAD:main"); err != nil {
		log.Printf("vault: push failed (committed locally): %v", err)
	}
}

// ensureRepo makes rootPath a git repo on first boot and reconciles the remote
// on EVERY boot — so setting VAULT_REPO_URL on a vault that's already a git repo
// (e.g. an existing Railway volume) actually wires up the push target instead of
// being silently skipped.
func ensureRepo() error {
	if _, err := os.Stat(filepath.Join(rootPath, ".git")); err != nil {
		if err := runGit("init", "-b", "main"); err != nil {
			return err
		}
	}
	if remoteURL != "" {
		// set-url updates an existing origin; if there is none yet, add it.
		if err := runGit("remote", "set-url", "origin", remoteURL); err != nil {
			if err := runGit("remote", "add", "origin", remoteURL); err != nil {
				log.Printf("vault: set remote failed: %v", err)
			}
		}
	}
	return nil
}

// setupSSH writes GIT_SSH_KEY to a private file outside the vault (so it never
// gets committed) and points git at it via GIT_SSH_COMMAND.
func setupSSH() error {
	key := os.Getenv("GIT_SSH_KEY")
	if key == "" {
		return nil
	}
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	keyPath := filepath.Join(os.TempDir(), "secondbrain_vault_key")
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		return err
	}
	gitEnv = append(gitEnv,
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new",
	)
	return nil
}

// writeIndexes regenerates an index.md in every vault directory so the bundle
// is a navigable OKF concept graph: each index links to its child concepts and
// sub-sections. Runs on the single writer goroutine, so no locking needed.
func writeIndexes() error {
	return filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}

		var sections, concepts []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				if name == ".git" {
					continue
				}
				sections = append(sections, name)
			} else if strings.HasSuffix(name, ".md") && name != "index.md" {
				concepts = append(concepts, name)
			}
		}
		sort.Strings(sections)
		sort.Strings(concepts)

		title := d.Name()
		if path == rootPath {
			title = "Vault"
		}

		var b strings.Builder
		b.WriteString("---\n")
		b.WriteString("type: \"Index\"\n")
		fmt.Fprintf(&b, "title: %q\n", title)
		b.WriteString("---\n\n")
		fmt.Fprintf(&b, "# %s\n\n", title)

		if len(sections) > 0 {
			b.WriteString("## Sections\n\n")
			for _, s := range sections {
				fmt.Fprintf(&b, "- [%s](%s/index.md)\n", s, s)
			}
			b.WriteString("\n")
		}
		if len(concepts) > 0 {
			b.WriteString("## Concepts\n\n")
			for _, c := range concepts {
				title, _ := readFrontmatter(filepath.Join(path, c))
				if title == "" {
					title = strings.TrimSuffix(c, ".md")
				}
				fmt.Fprintf(&b, "- [%s](%s)\n", title, c)
			}
			b.WriteString("\n")
		}

		return os.WriteFile(filepath.Join(path, "index.md"), []byte(b.String()), 0o644)
	})
}

func gitCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = rootPath
	cmd.Env = append(os.Environ(), gitEnv...)
	return cmd
}

func runGit(args ...string) error {
	out, err := gitCmd(args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hasStagedChanges reports whether the index differs from HEAD.
// `git diff --cached --quiet` exits 0 when clean, non-zero when there are changes.
func hasStagedChanges() bool {
	return gitCmd("diff", "--cached", "--quiet").Run() != nil
}
