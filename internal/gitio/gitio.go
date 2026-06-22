// Package gitio provides Git repository I/O operations using go-git.
package gitio

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"kai/internal/filesource"
)

// Repository wraps a go-git repository.
type Repository struct {
	repo *git.Repository
	path string
}

// GitSource implements filesource.FileSource for Git commits.
type GitSource struct {
	repo   *Repository
	commit *object.Commit
	files  []*filesource.FileInfo
}

// Open opens an existing Git repository.
func Open(repoPath string) (*Repository, error) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("opening repository: %w", err)
	}
	return &Repository{repo: repo, path: repoPath}, nil
}

// ResolveRef resolves a git reference (branch name, tag, or commit hash) to a commit.
func (r *Repository) ResolveRef(refName string) (*object.Commit, error) {
	// Try as a branch first
	ref, err := r.repo.Reference(plumbing.NewBranchReferenceName(refName), true)
	if err == nil {
		commit, err := r.repo.CommitObject(ref.Hash())
		if err != nil {
			return nil, fmt.Errorf("getting commit: %w", err)
		}
		return commit, nil
	}

	// Try as a tag
	ref, err = r.repo.Reference(plumbing.NewTagReferenceName(refName), true)
	if err == nil {
		commit, err := r.repo.CommitObject(ref.Hash())
		if err != nil {
			return nil, fmt.Errorf("getting commit: %w", err)
		}
		return commit, nil
	}

	// Try as a commit hash
	hash := plumbing.NewHash(refName)
	commit, err := r.repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("resolving ref %q: not a branch, tag, or commit hash", refName)
	}
	return commit, nil
}

// OpenSource opens a Git repository and resolves a ref to create a FileSource.
func OpenSource(repoPath, gitRef string) (*GitSource, error) {
	repo, err := Open(repoPath)
	if err != nil {
		return nil, err
	}

	commit, err := repo.ResolveRef(gitRef)
	if err != nil {
		return nil, err
	}

	gs := &GitSource{
		repo:   repo,
		commit: commit,
	}

	// Pre-load files
	if err := gs.loadFiles(); err != nil {
		return nil, err
	}

	return gs, nil
}

// GetFiles returns all supported source files from the commit.
func (gs *GitSource) GetFiles() ([]*filesource.FileInfo, error) {
	return gs.files, nil
}

// GetFile returns a specific file by path from the commit.
func (gs *GitSource) GetFile(path string) (*filesource.FileInfo, error) {
	tree, err := gs.commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("getting tree: %w", err)
	}

	f, err := tree.File(path)
	if err != nil {
		return nil, fmt.Errorf("getting file %s: %w", path, err)
	}

	reader, err := f.Reader()
	if err != nil {
		return nil, fmt.Errorf("opening file %s: %w", path, err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", path, err)
	}

	return &filesource.FileInfo{
		Path:    path,
		Content: content,
		Lang:    detectLang(path),
	}, nil
}

// Identifier returns the commit hash.
func (gs *GitSource) Identifier() string {
	return gs.commit.Hash.String()
}

// SourceType returns "git".
func (gs *GitSource) SourceType() string {
	return "git"
}

// Commit returns the underlying commit object (for Git-specific operations like diff).
func (gs *GitSource) Commit() *object.Commit {
	return gs.commit
}

// Repository returns the underlying repository (for Git-specific operations).
func (gs *GitSource) Repository() *Repository {
	return gs.repo
}

// loadFiles loads all files from the commit tree.
func (gs *GitSource) loadFiles() error {
	tree, err := gs.commit.Tree()
	if err != nil {
		return fmt.Errorf("getting tree: %w", err)
	}

	var files []*filesource.FileInfo
	err = tree.Files().ForEach(func(f *object.File) error {
		lang := detectLang(f.Name)

		reader, err := f.Reader()
		if err != nil {
			return fmt.Errorf("opening file %s: %w", f.Name, err)
		}
		defer reader.Close()

		content, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("reading file %s: %w", f.Name, err)
		}

		files = append(files, &filesource.FileInfo{
			Path:    f.Name,
			Content: content,
			Lang:    lang,
		})
		return nil
	})
	if err != nil {
		return err
	}

	gs.files = files
	return nil
}

// DiffFilesNative uses git diff --name-status for fast diffing on large repos.
func (r *Repository) DiffFilesNative(base, head string) (added, modified, deleted []string, err error) {
	cmd := exec.Command("git", "diff", "--name-status", base, head)
	cmd.Dir = r.path
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("git diff --name-status: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		status, path := parts[0], parts[1]
		switch {
		case strings.HasPrefix(status, "A"):
			added = append(added, path)
		case strings.HasPrefix(status, "D"):
			deleted = append(deleted, path)
		case strings.HasPrefix(status, "M"):
			modified = append(modified, path)
		case strings.HasPrefix(status, "R"):
			// Rename: tab-separated old\tnew in the path part
			renameParts := strings.SplitN(path, "\t", 2)
			if len(renameParts) == 2 {
				deleted = append(deleted, renameParts[0])
				added = append(added, renameParts[1])
			}
		}
	}
	return added, modified, deleted, nil
}

// DiffFiles returns the paths of files that differ between two commits.
func (r *Repository) DiffFiles(baseCommit, headCommit *object.Commit) (added, modified, deleted []string, err error) {
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("getting base tree: %w", err)
	}

	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("getting head tree: %w", err)
	}

	changes, err := baseTree.Diff(headTree)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("computing diff: %w", err)
	}

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			continue
		}

		switch action {
		case 1: // Insert
			if detectLang(change.To.Name) != "" {
				added = append(added, change.To.Name)
			}
		case 2: // Delete
			if detectLang(change.From.Name) != "" {
				deleted = append(deleted, change.From.Name)
			}
		case 0: // Modify
			if detectLang(change.From.Name) != "" {
				modified = append(modified, change.From.Name)
			}
		}
	}

	return added, modified, deleted, nil
}

// detectLang detects the language based on file extension.
func detectLang(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	// JavaScript/TypeScript
	case ".ts", ".tsx":
		return "ts"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "js"
	// Structured data
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	// Documentation
	case ".md", ".markdown":
		return "markdown"
	case ".txt", ".text":
		return "text"
	// Config
	case ".ini", ".cfg", ".conf":
		return "ini"
	case ".env":
		return "env"
	// Code
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc", ".cxx":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".php":
		return "php"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".sql":
		return "sql"
	case ".html", ".htm":
		return "html"
	case ".css", ".scss", ".sass", ".less":
		return "css"
	case ".proto":
		return "proto"
	case ".graphql", ".gql":
		return "graphql"
	default:
		return "blob"
	}
}

// GetCommitHash returns the hash of a commit as a string.
func GetCommitHash(commit *object.Commit) string {
	return commit.Hash.String()
}
