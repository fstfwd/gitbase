package gitbase

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sirupsen/logrus"
	"gopkg.in/src-d/go-billy-siva.v4"
	billy "gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/osfs"
	errors "gopkg.in/src-d/go-errors.v1"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/storage/filesystem"
	"gopkg.in/src-d/go-mysql-server.v0/sql"
)

var (
	errInvalidRepoKind       = errors.NewKind("invalid repo kind: %d")
	errRepoAlreadyRegistered = errors.NewKind("the repository is already registered: %s")
	errRepoCannotOpen        = errors.NewKind("the repository could not be opened: %s")
)

// Repository struct holds an initialized repository and its ID
type Repository struct {
	ID   string
	Repo *git.Repository
}

// NewRepository creates and initializes a new Repository structure
func NewRepository(id string, repo *git.Repository) *Repository {
	return &Repository{
		ID:   id,
		Repo: repo,
	}
}

// NewRepositoryFromPath creates and initializes a new Repository structure
// and initializes a go-git repository
func NewRepositoryFromPath(id, path string) (*Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	return NewRepository(id, repo), nil
}

// NewSivaRepositoryFromPath creates and initializes a new Repository structure
// and initializes a go-git repository backed by a siva file.
func NewSivaRepositoryFromPath(id, path string) (*Repository, error) {
	localfs := osfs.New(filepath.Dir(path))

	tmpDir, err := ioutil.TempDir(os.TempDir(), "gitbase-siva")
	if err != nil {
		return nil, err
	}

	tmpfs := osfs.New(tmpDir)

	fs, err := sivafs.NewFilesystem(localfs, filepath.Base(path), tmpfs)
	if err != nil {
		return nil, err
	}

	sto, err := filesystem.NewStorage(fs)
	if err != nil {
		return nil, err
	}

	repo, err := git.Open(sto, nil)
	if err != nil {
		return nil, err
	}

	return NewRepository(id, repo), nil
}

type repository interface {
	ID() string
	Repo() (*Repository, error)
	FS() (billy.Filesystem, error)
	Path() string
}

type gitRepository struct {
	id   string
	path string
}

func gitRepo(id, path string) repository {
	return &gitRepository{id, path}
}

func (r *gitRepository) ID() string {
	return r.id
}

func (r *gitRepository) Repo() (*Repository, error) {
	return NewRepositoryFromPath(r.id, r.path)
}

func (r *gitRepository) FS() (billy.Filesystem, error) {
	return osfs.New(r.path), nil
}

func (r *gitRepository) Path() string {
	return r.path
}

type sivaRepository struct {
	id   string
	path string
}

func sivaRepo(id, path string) repository {
	return &sivaRepository{id, path}
}

func (r *sivaRepository) ID() string {
	return r.id
}

func (r *sivaRepository) Repo() (*Repository, error) {
	return NewSivaRepositoryFromPath(r.id, r.path)
}

func (r *sivaRepository) FS() (billy.Filesystem, error) {
	localfs := osfs.New(filepath.Dir(r.path))

	tmpDir, err := ioutil.TempDir(os.TempDir(), "gitbase-siva")
	if err != nil {
		return nil, err
	}

	tmpfs := osfs.New(tmpDir)

	return sivafs.NewFilesystem(localfs, filepath.Base(r.path), tmpfs)
}

func (r *sivaRepository) Path() string {
	return r.path
}

// RepositoryPool holds a pool git repository paths and
// functionality to open and iterate them.
type RepositoryPool struct {
	repositories map[string]repository
	idOrder      []string
}

// NewRepositoryPool initializes a new RepositoryPool
func NewRepositoryPool() *RepositoryPool {
	return &RepositoryPool{
		repositories: make(map[string]repository),
	}
}

// Add inserts a new repository in the pool.
func (p *RepositoryPool) Add(repo repository) error {
	id := repo.ID()
	if r, ok := p.repositories[id]; ok {
		return errRepoAlreadyRegistered.New(r.Path())
	}

	p.idOrder = append(p.idOrder, id)
	p.repositories[id] = repo

	return nil
}

// AddGit checks if a git repository can be opened and adds it to the pool. It
// also sets its path as ID.
func (p *RepositoryPool) AddGit(path string) (string, error) {
	return p.AddGitWithID(path, path)
}

// AddGitWithID checks if a git repository can be opened and adds it to the
// pool. ID should be specified.
func (p *RepositoryPool) AddGitWithID(id, path string) (string, error) {
	_, err := git.PlainOpen(path)
	if err != nil {
		return "", errRepoCannotOpen.Wrap(err, path)
	}

	err = p.Add(gitRepo(id, path))
	if err != nil {
		return "", err
	}

	return path, nil
}

// AddDir adds all direct subdirectories from path as git repos. Prefix is the
// number of directories to strip from the ID.
func (p *RepositoryPool) AddDir(prefix int, path string) error {
	dirs, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	for _, f := range dirs {
		if f.IsDir() {
			pa := filepath.Join(path, f.Name())
			id := IDFromPath(prefix, pa)
			if _, err := p.AddGitWithID(id, pa); err != nil {
				logrus.WithFields(logrus.Fields{
					"id":    id,
					"path":  pa,
					"error": err,
				}).Error("repository could not be added")
			} else {
				logrus.WithField("path", pa).Debug("repository added")
			}
		}
	}

	return nil
}

// AddSivaDir adds to the repository pool all siva files found inside the given
// directory and in its children directories, but not the children of those
// directories.
func (p *RepositoryPool) AddSivaDir(path string) error {
	return p.addSivaDir(path, path, true)
}

func (p *RepositoryPool) addSivaDir(root, path string, recursive bool) error {
	dirs, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	for _, f := range dirs {
		if f.IsDir() && recursive {
			dirPath := filepath.Join(path, f.Name())
			if err := p.addSivaDir(root, dirPath, false); err != nil {
				return err
			}
		} else {
			p.addSivaFile(root, path, f)
		}
	}

	return nil
}

// AddSivaFile adds to the pool the given file if it's a siva repository,
// that is, has the .siva extension
func (p *RepositoryPool) AddSivaFile(id, path string) {
	file := filepath.Base(path)
	if !strings.HasSuffix(file, ".siva") {
		logrus.WithField("file", file).Warn("found a non-siva file")
	}

	p.Add(sivaRepo(id, path))
	logrus.WithField("file", file).Debug("repository added")
}

// addSivaFile adds to the pool the given file if it's a siva repository,
// that is, has the .siva extension.
func (p *RepositoryPool) addSivaFile(root, path string, f os.FileInfo) {
	var relativeFileName string
	if root == path {
		relativeFileName = f.Name()
	} else {
		relPath := strings.TrimPrefix(strings.Replace(path, root, "", -1), "/\\")
		relativeFileName = filepath.Join(relPath, f.Name())
	}

	if strings.HasSuffix(f.Name(), ".siva") {
		path := filepath.Join(path, f.Name())
		p.Add(sivaRepo(path, path))
		logrus.WithField("file", relativeFileName).Debug("repository added")
	} else {
		logrus.WithField("file", relativeFileName).Warn("found a non-siva file, skipping")
	}
}

// GetPos retrieves a repository at a given position. If the position is
// out of bounds it returns io.EOF.
func (p *RepositoryPool) GetPos(pos int) (*Repository, error) {
	if pos >= len(p.repositories) {
		return nil, io.EOF
	}

	id := p.idOrder[pos]
	if id == "" {
		return nil, io.EOF
	}

	return p.GetRepo(id)
}

// ErrPoolRepoNotFound is returned when a repository id is not present in the pool.
var ErrPoolRepoNotFound = errors.NewKind("repository id %s not found in the pool")

// GetRepo returns a repository with the given id from the pool.
func (p *RepositoryPool) GetRepo(id string) (*Repository, error) {
	r, ok := p.repositories[id]
	if !ok {
		return nil, ErrPoolRepoNotFound.New(id)
	}

	return r.Repo()
}

// RepoIter creates a new Repository iterator
func (p *RepositoryPool) RepoIter() (*RepositoryIter, error) {
	iter := &RepositoryIter{
		pool: p,
	}
	atomic.StoreInt32(&iter.pos, 0)

	return iter, nil
}

// RepositoryIter iterates over all repositories in the pool
type RepositoryIter struct {
	pos  int32
	pool *RepositoryPool
}

// Next retrieves the next Repository. It returns io.EOF as error
// when there are no more Repositories to retrieve.
func (i *RepositoryIter) Next() (*Repository, error) {
	pos := int(atomic.LoadInt32(&i.pos))
	r, err := i.pool.GetPos(pos)
	atomic.AddInt32(&i.pos, 1)

	return r, err
}

// Close finished iterator. It's no-op.
func (i *RepositoryIter) Close() error {
	return nil
}

// RowRepoIter is the interface needed by each iterator
// implementation
type RowRepoIter interface {
	NewIterator(*Repository) (RowRepoIter, error)
	Next() (sql.Row, error)
	Close() error
}

type iteratorBuilder func(*sql.Context, selectors, []sql.Expression) (RowRepoIter, error)

// RowRepoIter is used as the base to iterate over all the repositories
// in the pool
type rowRepoIter struct {
	mu sync.Mutex

	currRepoIter   RowRepoIter
	repositoryIter *RepositoryIter
	iter           RowRepoIter
	session        *Session
	ctx            *sql.Context
}

// NewRowRepoIter initializes a new repository iterator.
//
// * ctx: it should contain a gitbase.Session
// * iter: specific RowRepoIter interface
//     * NewIterator: called when a new repository is about to be iterated,
//         returns a new RowRepoIter
//     * Next: called for each row
//     * Close: called when a repository finished iterating
func NewRowRepoIter(
	ctx *sql.Context,
	iter RowRepoIter,
) (sql.RowIter, error) {
	s, ok := ctx.Session.(*Session)
	if !ok || s == nil {
		return nil, ErrInvalidGitbaseSession.New(ctx.Session)
	}

	rIter, err := s.Pool.RepoIter()
	if err != nil {
		return nil, err
	}

	repoIter := rowRepoIter{
		currRepoIter:   nil,
		repositoryIter: rIter,
		iter:           iter,
		session:        s,
		ctx:            ctx,
	}

	return &repoIter, nil
}

// Next gets the next row
func (i *rowRepoIter) Next() (sql.Row, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	for {
		select {
		case <-i.ctx.Done():
			return nil, ErrSessionCanceled.New()

		default:
			if i.currRepoIter == nil {
				repo, err := i.repositoryIter.Next()
				if err != nil {
					if err == io.EOF {
						return nil, io.EOF
					}

					if i.session.SkipGitErrors {
						continue
					}

					return nil, err
				}

				i.currRepoIter, err = i.iter.NewIterator(repo)
				if err != nil {
					if i.session.SkipGitErrors {
						continue
					}

					return nil, err
				}
			}

			row, err := i.currRepoIter.Next()
			if err != nil {
				if err == io.EOF {
					i.currRepoIter.Close()
					i.currRepoIter = nil
					continue
				}

				if i.session.SkipGitErrors {
					continue
				}

				return nil, err
			}

			return row, nil
		}
	}
}

// Close called to close the iterator
func (i *rowRepoIter) Close() error {
	if i.currRepoIter != nil {
		i.currRepoIter.Close()
	}
	return i.iter.Close()
}
