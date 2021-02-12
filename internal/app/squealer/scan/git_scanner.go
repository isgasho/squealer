package scan

import (
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	"io"
	"os"
	"sync"

	"github.com/owenrumney/squealer/internal/app/squealer/match"
	"github.com/owenrumney/squealer/internal/app/squealer/mertics"
)

type CommitFile struct {
	commit *object.Commit
	file   *object.File
}

type gitScanner struct {
	mc               match.MatcherController
	metrics          *mertics.Metrics
	workingDirectory string
	ignorePaths      []string
	fromHash         plumbing.Hash
	toHash           plumbing.Hash
	ignoreExtensions []string
	headSet          bool
	everything       bool
}

func (s *gitScanner) GetType() ScannerType {
	return GitScanner
}

func newGitScanner(sc ScannerConfig) (*gitScanner, error) {
	if _, err := os.Stat(sc.Basepath); err != nil {
		return nil, err
	}
	metrics := mertics.NewMetrics()
	mc := match.NewMatcherController(sc.Cfg, metrics, sc.Redacted)

	scanner := &gitScanner{
		mc:               *mc,
		metrics:          metrics,
		workingDirectory: sc.Basepath,
		ignorePaths:      sc.Cfg.IgnorePrefixes,
		ignoreExtensions: sc.Cfg.IgnoreExtensions,
		everything:       sc.Everything,
	}
	if len(sc.FromHash) > 0 {
		scanner.fromHash = plumbing.NewHash(sc.FromHash)
	}

	if len(sc.ToHash) > 0 {
		scanner.toHash = plumbing.NewHash(sc.ToHash)
		scanner.headSet = true
	}

	return scanner, nil
}

func (s *gitScanner) Scan() error {
	client, err := git.PlainOpen(s.workingDirectory)
	if err != nil {
		return err
	}

	commits, err := s.getRelevantCommitIter(client)
	if err != nil {
		return err
	}

	s.metrics.StartTimer()
	defer s.metrics.StopTimer()

	var ch = make(chan CommitFile, 50)
	var wg sync.WaitGroup

	defer func() {
		close(ch)
		wg.Wait()
	}()

	processes := 5
	wg.Add(processes)
	for i := 0; i < processes; i++ {
		go func() {
			for {
				cf, ok := <-ch
				if !ok {
					wg.Done()
					return
				}
				err := s.processFile(cf)
				if err != nil {
					log.WithError(err).Error(err.Error())
				}
			}
		}()
	}

	s.monitorSignals(processes, wg)

	commit, err := commits.Next()
	for err == nil && commit != nil {
		if err := s.processCommit(commit, ch); err != nil {
			log.WithError(err).Error(err.Error())
		}
		if commit.Hash.String() == s.fromHash.String() {
			log.Info("commit hash reached - stopping")
			// reached the starting commit - stop here
			return nil
		}
		commit, err = commits.Next()
		s.metrics.IncrementCommitsProcessed()
	}
	if err != nil && err != io.EOF {
		logrus.WithError(err).Error("error was not null or an EOF")
	}
	return nil
}

func (s *gitScanner) processCommit(commit *object.Commit, ch chan CommitFile) error {

	log.Debugf("commit: %s", commit.Hash.String())
	if len(commit.ParentHashes) == 0 {
		files, err := commit.Files()
		if err != nil {
			return err
		}
		err = files.ForEach(func(file *object.File) error {
			ch <- CommitFile{commit, file}
			return nil
		})
		return err
	}

	ctree, err := commit.Tree()
	if err != nil {
		return err
	}
	parent, err := commit.Parents().Next()
	if err != nil {
		return err
	}

	ptree, err := parent.Tree()
	if err != nil {
		return err
	}

	s.cleanTree(ctree)
	s.cleanTree(ptree)

	changes, err := ptree.Diff(ctree)
	if err != nil {
		return err
	}

	for _, change := range changes {
		_, toFile, err := change.Files()
		if err != nil {
			if err != io.EOF {
				return err
			}
			continue
		}

		ch <- CommitFile{commit, toFile}
	}

	return nil
}

func (s *gitScanner) cleanTree(tree *object.Tree) {
	var cleanEntries []object.TreeEntry
	for _, entry := range tree.Entries {
		if shouldIgnore(entry.Name, s.ignorePaths, s.ignoreExtensions) {
			continue
		}
		cleanEntries = append(cleanEntries, entry)
	}
	tree.Entries = cleanEntries
}

func (s *gitScanner) processFile(cf CommitFile) error {
	file := cf.file
	if file == nil {
		return nil
	}

	if isBin, err := file.IsBinary(); err != nil || isBin {
		return nil
	}
	if shouldIgnore(file.Name, s.ignorePaths, s.ignoreExtensions) {
		return nil
	}
	content, err := file.Contents()
	if err != nil {
		return err
	}

	err = s.mc.Evaluate(file.Name, content, cf.commit)
	s.metrics.IncrementFilesProcessed()
	return err
}

func (s *gitScanner) GetMetrics() *mertics.Metrics {
	return s.metrics
}

func (s *gitScanner) GetTransgressions() []match.Transgression {
	return s.mc.Transgressions()
}

func (s *gitScanner) getRelevantCommitIter(client *git.Repository) (object.CommitIter, error) {
	if s.everything {
		log.Info("you asked for everything.....")
		commits, err := client.CommitObjects()
		if err != nil {
			return nil, err
		}
		return commits, nil
	}

	var headRef plumbing.Hash
	if s.headSet {
		headRef = s.toHash

	} else {
		ref, _ := client.Head()
		if ref == nil {
			headRef = plumbing.ZeroHash
		} else {
			headRef = ref.Hash()
		}
	}

	var commits object.CommitIter
	var err error

	if headRef != plumbing.ZeroHash {
		logrus.Infof("starting at hash %s", headRef.String())
		commits, err = client.Log(&git.LogOptions{
			From:  headRef,
			All:   false,
			Order: git.LogOrderCommitterTime,
		})
		if err != nil {
			return nil, err
		}
	} else {
		logrus.Info("No head was found, scanning all commits")
		commits, err = client.CommitObjects()
		if err != nil {
			return nil, err
		}
	}
	return commits, err
}
