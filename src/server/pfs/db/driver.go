package persist

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	pfsserver "github.com/pachyderm/pachyderm/src/server/pfs"
	libclock "github.com/pachyderm/pachyderm/src/server/pfs/db/clock"
	"github.com/pachyderm/pachyderm/src/server/pfs/db/persist"
	"github.com/pachyderm/pachyderm/src/server/pfs/drive"

	"github.com/dancannon/gorethink"
	"github.com/gogo/protobuf/proto"
	"go.pedge.io/lion/proto"
	"go.pedge.io/pb/go/google/protobuf"
	"go.pedge.io/proto/time"
	"google.golang.org/grpc"
)

// A Table is a rethinkdb table name.
type Table string

// A PrimaryKey is a rethinkdb primary key identifier.
type PrimaryKey string

// Errors
type ErrCommitNotFound struct {
	error
}

type ErrBranchExists struct {
	error
}

const (
	repoTable   Table = "Repos"
	diffTable   Table = "Diffs"
	clockTable  Table = "Clocks"
	commitTable Table = "Commits"

	connectTimeoutSeconds = 5
)

const (
	ErrConflictFileTypeMsg = "file type conflict"
)

var (
	tables = []Table{
		repoTable,
		commitTable,
		diffTable,
		clockTable,
	}

	tableToTableCreateOpts = map[Table][]gorethink.TableCreateOpts{
		repoTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "Name",
			},
		},
		commitTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "ID",
			},
		},
		diffTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "ID",
			},
		},
		clockTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "ID",
			},
		},
	}
)

type driver struct {
	blockClient pfs.BlockAPIClient
	dbName      string
	dbClient    *gorethink.Session
}

func NewDriver(blockAddress string, dbAddress string, dbName string) (drive.Driver, error) {
	clientConn, err := grpc.Dial(blockAddress, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	dbClient, err := dbConnect(dbAddress)
	if err != nil {
		return nil, err
	}

	return &driver{
		blockClient: pfs.NewBlockAPIClient(clientConn),
		dbName:      dbName,
		dbClient:    dbClient,
	}, nil
}

func InitDB(address string, databaseName string) error {
	session, err := dbConnect(address)
	if err != nil {
		return err
	}
	defer session.Close()

	// Create the database
	if _, err := gorethink.DBCreate(databaseName).RunWrite(session); err != nil {
		return err
	}

	// Create tables
	for _, table := range tables {
		tableCreateOpts := tableToTableCreateOpts[table]
		if _, err := gorethink.DB(databaseName).TableCreate(table, tableCreateOpts...).RunWrite(session); err != nil {
			return err
		}
	}

	// Create indexes
	for _, someIndex := range Indexes {
		if _, err := gorethink.DB(databaseName).Table(someIndex.GetTable()).IndexCreateFunc(someIndex.GetName(), someIndex.GetCreateFunction(), someIndex.GetCreateOptions()).RunWrite(session); err != nil {
			return err
		}
		if _, err := gorethink.DB(databaseName).Table(someIndex.GetTable()).IndexWait(someIndex.GetName()).RunWrite(session); err != nil {
			return err
		}
	}
	return nil
}

func RemoveDB(address string, databaseName string) error {
	session, err := dbConnect(address)
	if err != nil {
		return err
	}
	defer session.Close()

	// Create the database
	if _, err := gorethink.DBDrop(databaseName).RunWrite(session); err != nil {
		return err
	}

	return nil
}

func dbConnect(address string) (*gorethink.Session, error) {
	return gorethink.Connect(gorethink.ConnectOpts{
		Address: address,
		Timeout: connectTimeoutSeconds * time.Second,
	})
}

func validateRepoName(name string) error {
	match, _ := regexp.MatchString("^[a-zA-Z0-9_]+$", name)

	if !match {
		return fmt.Errorf("repo name (%v) invalid: only alphanumeric and underscore characters allowed", name)
	}

	return nil
}

func (d *driver) getTerm(table Table) gorethink.Term {
	return gorethink.DB(d.dbName).Table(table)
}

func (d *driver) CreateRepo(repo *pfs.Repo, created *google_protobuf.Timestamp,
	provenance []*pfs.Repo, shards map[uint64]bool) error {
	err := validateRepoName(repo.Name)
	if err != nil {
		return err
	}

	_, err = d.getTerm(repoTable).Insert(&persist.Repo{
		Name:    repo.Name,
		Created: created,
	}).RunWrite(d.dbClient)
	return err
}

func (d *driver) InspectRepo(repo *pfs.Repo, shards map[uint64]bool) (repoInfo *pfs.RepoInfo, retErr error) {
	cursor, err := d.getTerm(repoTable).Get(repo.Name).Run(d.dbClient)
	if err != nil {
		return nil, err
	}
	rawRepo := &persist.Repo{}
	if err := cursor.One(rawRepo); err != nil {
		return nil, err
	}
	repoInfo = &pfs.RepoInfo{
		Repo:      &pfs.Repo{rawRepo.Name},
		Created:   rawRepo.Created,
		SizeBytes: rawRepo.Size,
	}
	return repoInfo, nil
}

func (d *driver) ListRepo(provenance []*pfs.Repo, shards map[uint64]bool) (repoInfos []*pfs.RepoInfo, retErr error) {
	cursor, err := d.getTerm(repoTable).Run(d.dbClient)
	if err != nil {
		return nil, err
	}
	var repos []*persist.Repo
	if err := cursor.All(&repos); err != nil {
		return nil, err
	}

	for _, repo := range repos {
		repoInfos = append(repoInfos, &pfs.RepoInfo{
			Repo:      &pfs.Repo{repo.Name},
			Created:   repo.Created,
			SizeBytes: repo.Size,
		})
	}
	return repoInfos, nil
}

func (d *driver) DeleteRepo(repo *pfs.Repo, shards map[uint64]bool, force bool) error {
	_, err := d.getTerm(repoTable).Get(repo.Name).Delete().RunWrite(d.dbClient)
	return err
}

func (d *driver) StartCommit(repo *pfs.Repo, commitID string, parentID string, branch string, started *google_protobuf.Timestamp, provenance []*pfs.Commit, shards map[uint64]bool) (retErr error) {
	var _provenance []string
	for _, c := range provenance {
		_provenance = append(_provenance, c.ID)
	}
	commit := &persist.Commit{
		ID:         commitID,
		Repo:       repo.Name,
		Started:    now(),
		Provenance: _provenance,
	}

	var clockID *persist.ClockID
	if parentID == "" {
		if branch == "" {
			branch = uuid.NewWithoutDashes()
		}
		for {
			// The head of this branch will be our parent commit
			parentCommit := &persist.Commit{}
			err := d.getHeadOfBranch(repo.Name, branch, parentCommit)
			if err != nil && err != gorethink.ErrEmptyResult {
				return err
			} else if err == gorethink.ErrEmptyResult {
				// we don't have a parent :(
				// so we create a new BranchClock
				commit.BranchClocks = libclock.NewBranchClocks(branch)
			} else {
				// we do have a parent :D
				// so we inherit our parent's branch clock for this particular branch,
				// and increment the last component by 1
				commit.BranchClocks, err = libclock.NewChildOfBranchClocks(parentCommit.BranchClocks, branch)
				if err != nil {
					return err
				}
			}
			clock, err := libclock.GetClockForBranch(commit.BranchClocks, branch)
			if err != nil {
				return err
			}
			clockID = getClockID(repo.Name, clock)
			err = d.insertMessage(clockTable, clockID)
			if gorethink.IsConflictErr(err) {
				// There is another process creating a commit on this branch
				// at the same time.  We lost the race, but we can try again
				continue
			} else if err != nil {
				return err
			}
			break
		}
	} else {
		parentCommit, err := d.getCommitByAmbiguousID(repo.Name, parentID)
		if err != nil {
			return err
		}

		// OBSOLETE
		parentBranch := parentCommit.BranchClocks[0].Clocks[len(parentCommit.BranchClocks[0].Clocks)-1].Branch

		if branch == "" {
			commit.BranchClocks = append(commit.BranchClocks, libclock.NewChild(parentCommit.BranchClocks[0]))
			branch = parentBranch
		} else {
			commit.BranchClocks, err = libclock.NewBranchOffBranchClocks(parentCommit.BranchClocks, parentBranch, branch)
			if err != nil {
				return err
			}
		}

		clock, err := libclock.GetClockForBranch(commit.BranchClocks, branch)
		if err != nil {
			return err
		}

		clockID = getClockID(repo.Name, clock)
		if err := d.insertMessage(clockTable, clockID); err != nil {
			if gorethink.IsConflictErr(err) {
				// This should only happen if there's another process creating the
				// very same branch at the same time, and we lost the race.
				return ErrBranchExists{fmt.Errorf("branch %s already exists", branch)}
			}
			return err
		}
	}
	defer func() {
		if retErr != nil {
			if err := d.deleteMessageByPrimaryKey(clockTable, clockID.ID); err != nil {
				protolion.Debugf("Unable to remove clock after StartCommit fails; this will result in database inconsistency")
			}
		}
	}()

	// TODO: what if the program exits here?  There will be an entry in the Clocks
	// table, but not in the Commits table.  Now you won't be able to create this
	// commit anymore.
	return d.insertMessage(commitTable, commit)
}

func (d *driver) getHeadOfBranch(repo string, branch string, commit *persist.Commit) error {
	cursor, err := d.betweenIndex(
		commitTable, CommitBranchIndex.GetName(),
		CommitBranchIndex.Key(repo, branch, 0),
		CommitBranchIndex.Key(repo, branch, gorethink.MaxVal),
		true,
	).Run(d.dbClient)
	if err != nil {
		return err
	}
	return cursor.One(commit)
}

func getClockID(repo string, c *persist.Clock) *persist.ClockID {
	return &persist.ClockID{
		ID:     fmt.Sprintf("%s/%s/%d", repo, c.Branch, c.Clock),
		Repo:   repo,
		Branch: c.Branch,
		Clock:  c.Clock,
	}
}

// parseClock takes a string of the form "branch/clock"
// and returns a Clock object.
// For example:
// "master/0" -> Clock{"master", 0}
func parseClock(clock string) (*persist.Clock, error) {
	parts := strings.Split(clock, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid commit ID %s")
	}
	c, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid commit ID %s")
	}
	return &persist.Clock{
		Branch: parts[0],
		Clock:  uint64(c),
	}, nil
}

type CommitChangeFeed struct {
	NewVal *persist.Commit `gorethink:"new_val,omitempty"`
}

// Given a commitID (database primary key), compute the size of the commit
// using diffs.
func (d *driver) computeCommitSize(commitID string) (uint64, error) {
	cursor, err := d.getTerm(diffTable).GetAllByIndex(
		DiffCommitIndex.GetName(),
		commitID,
	).Reduce(func(left, right gorethink.Term) gorethink.Term {
		return left.Merge(map[string]interface{}{
			"Size": left.Field("Size").Add(right.Field("Size")),
		})
	}).Default(&persist.Diff{}).Run(d.dbClient)
	if err != nil {
		return 0, err
	}

	var diff persist.Diff
	if err := cursor.One(&diff); err != nil {
		return 0, err
	}

	return diff.Size, nil
}

// FinishCommit blocks until its parent has been finished/cancelled
func (d *driver) FinishCommit(commit *pfs.Commit, finished *google_protobuf.Timestamp, cancel bool, shards map[uint64]bool) error {
	rawCommit, err := d.getCommitByAmbiguousID(commit.Repo.Name, commit.ID)
	if err != nil {
		return err
	}

	rawCommit.Size, err = d.computeCommitSize(rawCommit.ID)
	if err != nil {
		return err
	}

	// OBSOLETE
	// Once we move to the new implementation, we should be able to directly
	// Infer the parentID using the given commit ID.
	parentID, err := d.getIDOfParentCommit(commit.Repo.Name, commit.ID)
	if err != nil {
		return err
	}

	var parentCancelled bool
	if parentID != "" {
		cursor, err := d.getTerm(commitTable).Get(parentID).Changes(gorethink.ChangesOpts{
			IncludeInitial: true,
		}).Run(d.dbClient)

		if err != nil {
			return err
		}

		var change CommitChangeFeed
		for cursor.Next(&change) {
			if change.NewVal != nil && change.NewVal.Finished != nil {
				parentCancelled = change.NewVal.Cancelled
				break
			}
		}
		if err = cursor.Err(); err != nil {
			return err
		}
	}

	// Update the size of the repo.  Note that there is a consistency issue here:
	// If this transaction succeeds but the next one (updating Commit) fails,
	// then the repo size will be wrong.  TODO
	_, err = d.getTerm(repoTable).Get(rawCommit.Repo).Update(map[string]interface{}{
		"Size": gorethink.Row.Field("Size").Add(rawCommit.Size),
	}).RunWrite(d.dbClient)
	if err != nil {
		return err
	}

	rawCommit.Finished = finished
	rawCommit.Cancelled = parentCancelled || cancel
	_, err = d.getTerm(commitTable).Get(rawCommit.ID).Update(rawCommit).RunWrite(d.dbClient)

	return err
}

func (d *driver) InspectCommit(commit *pfs.Commit, shards map[uint64]bool) (commitInfo *pfs.CommitInfo, retErr error) {
	rawCommit, err := d.getCommitByAmbiguousID(commit.Repo.Name, commit.ID)
	if err != nil {
		return nil, err
	}
	commitInfo = rawCommitToCommitInfo(rawCommit)
	if commitInfo.Finished == nil {
		commitInfo.SizeBytes, err = d.computeCommitSize(rawCommit.ID)
		if err != nil {
			return nil, err
		}
	}
	// OBSOLETE
	// Old API Server expects request commit ID to match results commit ID
	commitInfo.Commit.ID = commit.ID
	return commitInfo, nil
}

func rawCommitToCommitInfo(rawCommit *persist.Commit) *pfs.CommitInfo {
	commitType := pfs.CommitType_COMMIT_TYPE_READ
	var branch string
	if len(rawCommit.BranchClocks) > 0 {
		branch = libclock.GetBranchNameFromBranchClock(rawCommit.BranchClocks[0])
	}
	if rawCommit.Finished == nil {
		commitType = pfs.CommitType_COMMIT_TYPE_WRITE
	}
	return &pfs.CommitInfo{
		Commit: &pfs.Commit{
			Repo: &pfs.Repo{rawCommit.Repo},
			ID:   rawCommit.ID,
		},
		Branch:     branch,
		Started:    rawCommit.Started,
		Finished:   rawCommit.Finished,
		Cancelled:  rawCommit.Cancelled,
		CommitType: commitType,
		SizeBytes:  rawCommit.Size,
	}
}

func (d *driver) ListCommit(repos []*pfs.Repo, commitType pfs.CommitType, fromCommits []*pfs.Commit, provenance []*pfs.Commit, all bool, shards map[uint64]bool, block bool) ([]*pfs.CommitInfo, error) {
	repoToFromCommit := make(map[string]string)
	for _, repo := range repos {
		repoToFromCommit[repo.Name] = ""
	}
	for _, commit := range fromCommits {
		repoToFromCommit[commit.Repo.Name] = commit.ID
	}
	var queries []interface{}
	for repo, commit := range repoToFromCommit {
		if commit == "" {
			// List all commits in the repo, ordered within each branch
			branches, err := d.ListBranch(&pfs.Repo{Name: repo}, nil)
			if err != nil {
				return nil, err
			}
			for _, branch := range branches {
				queries = append(queries, d.getTerm(commitTable).OrderBy(gorethink.OrderByOpts{
					Index: gorethink.Desc(CommitBranchIndex.GetName()),
				}).Between(CommitBranchIndex.Key(repo, branch.Branch, gorethink.MinVal), CommitBranchIndex.Key(repo, branch.Branch, gorethink.MaxVal), gorethink.BetweenOpts{
					Index: CommitBranchIndex.GetName(),
				}))
			}
		} else {
			branchClock, err := d.getClockByAmbiguousID(repo, commit)
			if err != nil {
				return nil, err
			}
			lastClock := branchClock.Clocks[len(branchClock.Clocks)-1]
			queries = append(queries, d.getTerm(commitTable).OrderBy(gorethink.OrderByOpts{
				Index: gorethink.Desc(CommitBranchIndex.GetName()),
			}).Between(CommitBranchIndex.Key(repo, lastClock.Branch, lastClock.Clock+1), CommitBranchIndex.Key(repo, lastClock.Branch, gorethink.MaxVal), gorethink.BetweenOpts{
				Index: CommitBranchIndex.GetName(),
			}))
		}
	}
	query := gorethink.Union(queries...)
	if !all {
		query = query.Filter(map[string]interface{}{
			"Cancelled": false,
		})
	}
	switch commitType {
	case pfs.CommitType_COMMIT_TYPE_READ:
		query = query.Filter(func(commit gorethink.Term) gorethink.Term {
			return commit.Field("Finished").Ne(nil)
		})
	case pfs.CommitType_COMMIT_TYPE_WRITE:
		query = query.Filter(func(commit gorethink.Term) gorethink.Term {
			return commit.Field("Finished").Eq(nil)
		})
	}
	var provenanceIDs []interface{}
	for _, commit := range provenance {
		c, err := d.getCommitByAmbiguousID(commit.Repo.Name, commit.ID)
		if err != nil {
			return nil, err
		}
		provenanceIDs = append(provenanceIDs, c.ID)
	}
	if provenanceIDs != nil {
		query = query.Filter(func(commit gorethink.Term) gorethink.Term {
			return commit.Field("Provenance").Contains(provenanceIDs...)
		})
	}

	cursor, err := query.Run(d.dbClient)
	if err != nil {
		return nil, err
	}
	var commits []*persist.Commit
	if err := cursor.All(&commits); err != nil {
		return nil, err
	}

	var commitInfos []*pfs.CommitInfo
	if len(commits) > 0 {
		for _, commit := range commits {
			commitInfos = append(commitInfos, rawCommitToCommitInfo(commit))
		}
	} else if block {
		query = query.Changes(gorethink.ChangesOpts{
			IncludeInitial: true,
		}).Field("new_val")
		cursor, err := query.Run(d.dbClient)
		if err != nil {
			return nil, err
		}
		var commit persist.Commit
		cursor.Next(&commit)
		if err := cursor.Err(); err != nil {
			return nil, err
		}
		commitInfos = append(commitInfos, rawCommitToCommitInfo(&commit))
	}

	return commitInfos, nil
}

func (d *driver) ListBranch(repo *pfs.Repo, shards map[uint64]bool) ([]*pfs.CommitInfo, error) {
	// Get all branches
	cursor, err := d.getTerm(clockTable).Between(
		[]interface{}{repo.Name, gorethink.MinVal},
		[]interface{}{repo.Name, gorethink.MaxVal},
		gorethink.BetweenOpts{
			Index: ClockBranchIndex.GetName(),
		},
	).Field("Branch").Distinct().Run(d.dbClient)
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	var branches []string
	if err := cursor.All(&branches); err != nil {
		return nil, err
	}

	// OBSOLETE
	// To maintain API compatibility, we return the heads of the branches
	var commitInfos []*pfs.CommitInfo
	for _, branch := range branches {
		commit := &persist.Commit{}
		if err := d.getHeadOfBranch(repo.Name, branch, commit); err != nil {
			return nil, err
		}
		commitInfos = append(commitInfos, &pfs.CommitInfo{
			Commit: &pfs.Commit{
				Repo: repo,
				ID:   commit.ID,
			},
			Branch: branch,
		})
	}
	return commitInfos, nil
}

func (d *driver) DeleteCommit(commit *pfs.Commit, shards map[uint64]bool) error {
	return errors.New("DeleteCommit is not supported")
}

// checkFileType returns an error if the given type conflicts with the preexisting
// type.  TODO: cache file types
func (d *driver) checkFileType(repo string, commit string, path string, typ persist.FileType) (err error) {
	diff, err := d.inspectFile(&pfs.File{
		Commit: &pfs.Commit{
			Repo: &pfs.Repo{
				Name: repo,
			},
			ID: commit,
		},
		Path: path,
	}, nil, nil)
	if err != nil {
		_, ok := err.(*pfsserver.ErrFileNotFound)
		if ok {
			// If the file was not found, then there's no type conflict
			return nil
		}
		return err
	}
	if diff.FileType != typ && diff.FileType != persist.FileType_NONE {
		return errors.New(ErrConflictFileTypeMsg)
	}
	return nil
}

func (d *driver) PutFile(file *pfs.File, handle string,
	delimiter pfs.Delimiter, shard uint64, reader io.Reader) (retErr error) {
	fixPath(file)
	// TODO: eventually optimize this with a cache so that we don't have to
	// go to the database to figure out if the commit exists
	commit, err := d.getCommitByAmbiguousID(file.Commit.Repo.Name, file.Commit.ID)
	if err != nil {
		return err
	}

	_client := client.APIClient{BlockAPIClient: d.blockClient}
	blockrefs, err := _client.PutBlock(delimiter, reader)
	if err != nil {
		return err
	}

	var refs []*persist.BlockRef
	var size uint64
	for _, blockref := range blockrefs.BlockRef {
		ref := &persist.BlockRef{
			Hash:  blockref.Block.Hash,
			Upper: blockref.Range.Upper,
			Lower: blockref.Range.Lower,
		}
		refs = append(refs, ref)
		size += ref.Size()
	}

	var diffs []*persist.Diff
	// the ancestor directories
	for _, prefix := range getPrefixes(file.Path) {
		diffs = append(diffs, &persist.Diff{
			ID:           getDiffID(commit.ID, prefix),
			Repo:         commit.Repo,
			Delete:       false,
			CommitID:     commit.ID,
			Path:         prefix,
			BranchClocks: commit.BranchClocks,
			FileType:     persist.FileType_DIR,
			Modified:     now(),
		})
	}

	// the file itself
	diffs = append(diffs, &persist.Diff{
		ID:           getDiffID(commit.ID, file.Path),
		Repo:         commit.Repo,
		Delete:       false,
		CommitID:     commit.ID,
		Path:         file.Path,
		BlockRefs:    refs,
		Size:         size,
		BranchClocks: commit.BranchClocks,
		FileType:     persist.FileType_FILE,
		Modified:     now(),
	})

	// Make sure that there's no type conflict
	for _, diff := range diffs {
		if err := d.checkFileType(diff.Repo, diff.CommitID, diff.Path, diff.FileType); err != nil {
			return err
		}
	}

	// Actually, we don't know if Rethink actually inserts these documents in
	// order.  If it doesn't, then we might end up with "/foo/bar" but not
	// "/foo", which is kinda problematic.
	_, err = d.getTerm(diffTable).Insert(diffs, gorethink.InsertOpts{
		Conflict: func(id gorethink.Term, oldDoc gorethink.Term, newDoc gorethink.Term) gorethink.Term {
			return gorethink.Branch(
				// We throw an error if the new diff is of a different file type
				// than the old diff, unless the old diff is NONE
				oldDoc.Field("FileType").Ne(persist.FileType_NONE).And(oldDoc.Field("FileType").Ne(newDoc.Field("FileType"))),
				gorethink.Error(ErrConflictFileTypeMsg),
				oldDoc.Merge(map[string]interface{}{
					"BlockRefs": oldDoc.Field("BlockRefs").Add(newDoc.Field("BlockRefs")),
					"Size":      oldDoc.Field("Size").Add(newDoc.Field("Size")),
					// Overwrite the file type in case the old file type is NONE
					"FileType": newDoc.Field("FileType"),
					// Update modification time
					"Modified": newDoc.Field("Modified"),
				}),
			)
		},
	}).RunWrite(d.dbClient)
	return err
}

func now() *google_protobuf.Timestamp {
	return prototime.TimeToTimestamp(time.Now())
}

func getPrefixes(path string) []string {
	prefix := ""
	parts := strings.Split(path, "/")
	var res []string
	// skip the last part; we only want prefixes
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] != "" {
			prefix += "/" + parts[i]
			res = append(res, prefix)
		}
	}
	return res
}

func getDiffID(commitID string, path string) string {
	return fmt.Sprintf("%s:%s", commitID, path)
}

// the equivalent of above except that commitID is a rethink term
func getDiffIDFromTerm(commitID gorethink.Term, path string) gorethink.Term {
	return commitID.Add(":" + path)
}

func (d *driver) MakeDirectory(file *pfs.File, shard uint64) (retErr error) {
	return nil
}

func reverseSlice(s [][]*persist.BranchClock) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// fixPath prepends a slash to the file path if there isn't one,
// and removes the trailing slash if there is one.
func fixPath(file *pfs.File) {
	if len(file.Path) == 0 || file.Path[0] != '/' {
		file.Path = "/" + file.Path
	}
	if len(file.Path) > 1 && file.Path[len(file.Path)-1] == '/' {
		file.Path = file.Path[:len(file.Path)-1]
	}
}

func (d *driver) GetFile(file *pfs.File, filterShard *pfs.Shard, offset int64,
	size int64, from *pfs.Commit, shard uint64, unsafe bool, handle string) (io.ReadCloser, error) {
	fixPath(file)
	diff, err := d.inspectFile(file, filterShard, from)
	if err != nil {
		return nil, err
	}
	if diff.FileType == persist.FileType_DIR {
		return nil, fmt.Errorf("file %s/%s/%s is directory", file.Commit.Repo.Name, file.Commit.ID, file.Path)
	}
	return d.newFileReader(diff.BlockRefs, file, offset, size), nil
}

type fileReader struct {
	blockClient pfs.BlockAPIClient
	reader      io.Reader
	offset      int64
	size        int64 // how much data to read
	sizeRead    int64 // how much data has been read
	blockRefs   []*persist.BlockRef
	file        *pfs.File
}

func (d *driver) newFileReader(blockRefs []*persist.BlockRef, file *pfs.File, offset int64, size int64) *fileReader {
	return &fileReader{
		blockClient: d.blockClient,
		blockRefs:   blockRefs,
		offset:      offset,
		size:        size,
		file:        file,
	}
}

func filterBlockRefs(filterShard *pfs.Shard, file *pfs.File, blockRefs []*persist.BlockRef) []*persist.BlockRef {
	var result []*persist.BlockRef
	for _, blockRef := range blockRefs {
		if pfsserver.BlockInShard(filterShard, file, &pfs.Block{
			Hash: blockRef.Hash,
		}) {
			result = append(result, blockRef)
		}
	}
	return result
}

func (r *fileReader) Read(data []byte) (int, error) {
	var err error
	if r.reader == nil {
		var blockRef *persist.BlockRef
		for {
			if len(r.blockRefs) == 0 {
				return 0, io.EOF
			}
			blockRef = r.blockRefs[0]
			r.blockRefs = r.blockRefs[1:]
			blockSize := int64(blockRef.Size())
			if r.offset >= blockSize {
				r.offset -= blockSize
				continue
			}
			break
		}
		client := client.APIClient{BlockAPIClient: r.blockClient}
		r.reader, err = client.GetBlock(blockRef.Hash, uint64(r.offset), uint64(r.size))
		if err != nil {
			return 0, err
		}
		r.offset = 0
	}
	size, err := r.reader.Read(data)
	if err != nil && err != io.EOF {
		return size, err
	}
	if err == io.EOF {
		r.reader = nil
	}
	r.sizeRead += int64(size)
	if r.sizeRead == r.size {
		return size, io.EOF
	}
	if r.size > 0 && r.sizeRead > r.size {
		return 0, fmt.Errorf("read more than we need; this is likely a bug")
	}
	return size, nil
}

func (r *fileReader) Close() error {
	return nil
}

func (d *driver) InspectFile(file *pfs.File, filterShard *pfs.Shard, from *pfs.Commit, shard uint64, unsafe bool, handle string) (*pfs.FileInfo, error) {
	fixPath(file)
	diff, err := d.inspectFile(file, filterShard, from)
	if err != nil {
		return nil, err
	}

	res := &pfs.FileInfo{
		File: file,
	}

	// The most recent diff will tell us about the file type.
	// Technically any diff will do, but the very first diff might be a
	// deletion.
	switch diff.FileType {
	case persist.FileType_FILE:
		res.FileType = pfs.FileType_FILE_TYPE_REGULAR
		res.Modified = diff.Modified
		res.CommitModified = &pfs.Commit{
			Repo: file.Commit.Repo,
			ID:   diff.CommitID,
		}
		res.SizeBytes = diff.Size
	case persist.FileType_DIR:
		res.FileType = pfs.FileType_FILE_TYPE_DIR
		res.Modified = diff.Modified
		childrenDiffs, err := d.getChildren(file.Commit.Repo.Name, file.Path, from, file.Commit)
		if err != nil {
			return nil, err
		}
		for _, diff := range childrenDiffs {
			res.Children = append(res.Children, &pfs.File{
				Commit: &pfs.Commit{file.Commit.Repo, diff.CommitID},
				Path:   diff.Path,
			})
		}
	case persist.FileType_NONE:
		return nil, pfsserver.NewErrFileNotFound(file.Path, file.Commit.Repo.Name, file.Commit.ID)
	default:
		return nil, fmt.Errorf("unrecognized file type: %d; this is likely a bug", diff.FileType)
	}
	return res, nil
}

func (d *driver) Merge(from []*pfs.Commit, parent *pfs.Commit, strategy pfs.MergeStrategy) (*pfs.Commits, error) {
	return &pfs.Commits{}, nil
}

// foldDiffs takes an unordered stream of diffs for a given path, and return
// a single diff that represents the aggregation of these diffs.
func foldDiffs(diffs gorethink.Term) gorethink.Term {
	return diffs.Map(func(diff gorethink.Term) interface{} {
		return []interface{}{diff}
	}).Reduce(func(left gorethink.Term, right gorethink.Term) gorethink.Term {
		return left.Union(right).OrderBy(func(diff gorethink.Term) gorethink.Term {
			return persist.BranchClockToArray(diff.Field("BranchClocks").Nth(0))
		})
	}).Default([]interface{}{}).Fold(gorethink.Expr(&persist.Diff{}), func(acc gorethink.Term, diff gorethink.Term) gorethink.Term {
		// TODO: the fold function can easily take offset and size into account,
		// only returning blockrefs that fall into the range specified by offset
		// and size.
		return gorethink.Branch(
			diff.Field("Delete"),
			acc.Merge(map[string]interface{}{
				"Path":      diff.Field("Path"),
				"CommitID":  diff.Field("CommitID"),
				"BlockRefs": diff.Field("BlockRefs"),
				"FileType":  diff.Field("FileType"),
				"Size":      diff.Field("Size"),
				"Modified":  diff.Field("Modified"),
			}),
			acc.Merge(map[string]interface{}{
				"Path":      diff.Field("Path"),
				"CommitID":  diff.Field("CommitID"),
				"BlockRefs": acc.Field("BlockRefs").Add(diff.Field("BlockRefs")),
				"Size":      acc.Field("Size").Add(diff.Field("Size")),
				"FileType":  diff.Field("FileType"),
				"Modified":  diff.Field("Modified"),
			}),
		)
	})
}

func (d *driver) getChildren(repo string, parent string, fromCommit *pfs.Commit, toCommit *pfs.Commit) ([]*persist.Diff, error) {
	query, err := d.getIntervalQueryFromCommitRange(fromCommit, toCommit)
	if err != nil {
		return nil, err
	}

	cursor, err := foldDiffs(query.Map(func(clocks gorethink.Term) interface{} {
		return DiffParentIndex.Key(repo, parent, clocks)
	}).EqJoin(func(x gorethink.Term) gorethink.Term {
		// no-op
		return x
	}, d.getTerm(diffTable), gorethink.EqJoinOpts{
		Index: DiffParentIndex.GetName(),
	}).Field("right").Group("Path")).Ungroup().Field("reduction").Filter(func(diff gorethink.Term) gorethink.Term {
		return diff.Field("FileType").Ne(persist.FileType_NONE)
	}).OrderBy("Path").Run(d.dbClient)
	if err != nil {
		return nil, err
	}

	var diffs []*persist.Diff
	if err := cursor.All(&diffs); err != nil {
		return nil, err
	}
	return diffs, nil
}

func (d *driver) getChildrenRecursive(repo string, parent string, fromCommit *pfs.Commit, toCommit *pfs.Commit) ([]*persist.Diff, error) {
	query, err := d.getIntervalQueryFromCommitRange(fromCommit, toCommit)
	if err != nil {
		return nil, err
	}

	cursor, err := foldDiffs(query.Map(func(clocks gorethink.Term) interface{} {
		return DiffPrefixIndex.Key(repo, parent, clocks)
	}).EqJoin(func(x gorethink.Term) gorethink.Term {
		// no-op
		return x
	}, d.getTerm(diffTable), gorethink.EqJoinOpts{
		Index: DiffPrefixIndex.GetName(),
	}).Field("right").Group("Path")).Ungroup().Field("reduction").Filter(func(diff gorethink.Term) gorethink.Term {
		return diff.Field("FileType").Ne(persist.FileType_NONE)
	}).Group(func(diff gorethink.Term) gorethink.Term {
		// This query gives us the first component after the parent prefix.
		// For instance, if the path is "/foo/bar/buzz" and parent is "/foo",
		// this query gives us "bar".
		return diff.Field("Path").Split(parent, 1).Nth(1).Split("/").Nth(1)
	}).Reduce(func(left, right gorethink.Term) gorethink.Term {
		// Basically, we add up the sizes and discard the diff with the longer
		// path.  That way, we will be left with the diff with the shortest path,
		// namely the direct child of parent.
		return gorethink.Branch(
			left.Field("Path").Lt(right.Field("Path")),
			left.Merge(map[string]interface{}{
				"Size": left.Field("Size").Add(right.Field("Size")),
			}),
			right.Merge(map[string]interface{}{
				"Size": left.Field("Size").Add(right.Field("Size")),
			}),
		)
	}).Ungroup().Field("reduction").OrderBy("Path").Run(d.dbClient)
	if err != nil {
		return nil, err
	}

	var diffs []*persist.Diff
	if err := cursor.All(&diffs); err != nil {
		return nil, err
	}

	return diffs, nil
}

// intervalsToClocks takes a [fromClock, toClock] interval and returns an array
// of clocks in between this range.
// If reverse is set to true, the clocks will be in reverse order.
func intervalToClocks(fromClock *persist.BranchClock, toClock *persist.BranchClock, reverse bool) (gorethink.Term, error) {
	// Find the most recent diff that removes the path
	intervals, err := libclock.GetClockIntervals(fromClock, toClock)
	if err != nil {
		return gorethink.Term{}, err
	}

	if reverse {
		reverseSlice(intervals)
		return gorethink.Expr(intervals).ConcatMap(func(interval gorethink.Term) gorethink.Term {
			firstClock := persist.BranchClockToArray(interval.Nth(0))
			secondClock := persist.BranchClockToArray(interval.Nth(1))
			return gorethink.Range(gorethink.Expr(0).Sub(secondClock.Nth(-1).Nth(1)), gorethink.Expr(0).Sub(firstClock.Nth(-1).Nth(1)).Add(1)).Map(func(x gorethink.Term) gorethink.Term {
				return firstClock.ChangeAt(-1, firstClock.Nth(-1).ChangeAt(1, gorethink.Expr(0).Sub(x)))
			})
		}), nil
	} else {
		return gorethink.Expr(intervals).ConcatMap(func(interval gorethink.Term) gorethink.Term {
			firstClock := persist.BranchClockToArray(interval.Nth(0))
			secondClock := persist.BranchClockToArray(interval.Nth(1))
			return gorethink.Range(firstClock.Nth(-1).Nth(1), secondClock.Nth(-1).Nth(1).Add(1)).Map(func(x gorethink.Term) gorethink.Term {
				return firstClock.ChangeAt(-1, firstClock.Nth(-1).ChangeAt(1, x))
			})
		}), nil
	}
}

// OBSOLETE
// This only works if the commit only belongs on one branch.
func (d *driver) getClockByAmbiguousID(repo string, commitID string) (*persist.BranchClock, error) {
	commit, err := d.getCommitByAmbiguousID(repo, commitID)
	if err != nil {
		return nil, err
	}
	return commit.BranchClocks[0], nil
}

func (d *driver) inspectFile(file *pfs.File, filterShard *pfs.Shard, from *pfs.Commit) (*persist.Diff, error) {
	if !pfsserver.FileInShard(filterShard, file) {
		return nil, pfsserver.NewErrFileNotFound(file.Path, file.Commit.Repo.Name, file.Commit.ID)
	}

	query, err := d.getIntervalQueryFromCommitRange(from, file.Commit)
	if err != nil {
		return nil, err
	}

	cursor, err := foldDiffs(query.Map(func(clocks gorethink.Term) interface{} {
		return DiffPathIndex.Key(file.Commit.Repo.Name, file.Path, clocks)
	}).EqJoin(func(x gorethink.Term) gorethink.Term {
		// no-op
		return x
	}, d.getTerm(diffTable), gorethink.EqJoinOpts{
		Index: DiffPathIndex.GetName(),
	}).Field("right")).Run(d.dbClient)
	if err != nil {
		return nil, err
	}

	diff := &persist.Diff{}
	if err := cursor.One(diff); err != nil {
		if err == gorethink.ErrEmptyResult {
			return nil, pfsserver.NewErrFileNotFound(file.Path, file.Commit.Repo.Name, file.Commit.ID)
		}
		return nil, err
	}

	if len(diff.BlockRefs) == 0 {
		// If the file is empty, we want to make sure that it's seen by one shard.
		if !pfsserver.BlockInShard(filterShard, file, nil) {
			return nil, pfsserver.NewErrFileNotFound(file.Path, file.Commit.Repo.Name, file.Commit.ID)
		}
	} else {
		// If the file is not empty, we want to make sure to return NotFound if
		// all blocks have been filtered out.
		diff.BlockRefs = filterBlockRefs(filterShard, file, diff.BlockRefs)
		if len(diff.BlockRefs) == 0 {
			return nil, pfsserver.NewErrFileNotFound(file.Path, file.Commit.Repo.Name, file.Commit.ID)
		}
	}

	return diff, nil
}

// getIntervalQueryFromCommitRange takes a commit range and returns a RethinkDB
// query that represents a stream of clocks in the commit range.
func (d *driver) getIntervalQueryFromCommitRange(fromCommit *pfs.Commit, toCommit *pfs.Commit) (gorethink.Term, error) {
	var err error
	var fromClock *persist.BranchClock
	if fromCommit != nil {
		fromClock, err = d.getClockByAmbiguousID(fromCommit.Repo.Name, fromCommit.ID)
		if err != nil {
			return gorethink.Term{}, err
		}
	}

	toClock, err := d.getClockByAmbiguousID(toCommit.Repo.Name, toCommit.ID)
	if err != nil {
		return gorethink.Term{}, err
	}

	query, err := intervalToClocks(fromClock, toClock, true)
	if err != nil {
		return gorethink.Term{}, err
	}

	return query, nil
}

func (d *driver) ListFile(file *pfs.File, filterShard *pfs.Shard, from *pfs.Commit, shard uint64, recurse bool, unsafe bool, handle string) ([]*pfs.FileInfo, error) {
	fixPath(file)
	// We treat the root directory specially: we know that it's a directory
	if file.Path != "/" {
		fileInfo, err := d.InspectFile(file, filterShard, from, shard, unsafe, handle)
		if err != nil {
			return nil, err
		}
		switch fileInfo.FileType {
		case pfs.FileType_FILE_TYPE_REGULAR:
			return []*pfs.FileInfo{fileInfo}, nil
		case pfs.FileType_FILE_TYPE_DIR:
			break
		default:
			return nil, fmt.Errorf("unrecognized file type %d; this is likely a bug", fileInfo.FileType)
		}
	}

	var diffs []*persist.Diff
	var err error
	if recurse {
		diffs, err = d.getChildrenRecursive(file.Commit.Repo.Name, file.Path, from, file.Commit)
	} else {
		diffs, err = d.getChildren(file.Commit.Repo.Name, file.Path, from, file.Commit)
	}
	if err != nil {
		return nil, err
	}

	var fileInfos []*pfs.FileInfo
	for _, diff := range diffs {
		fileInfo := &pfs.FileInfo{}
		fileInfo.File = &pfs.File{
			Commit: file.Commit,
			Path:   diff.Path,
		}
		fileInfo.SizeBytes = diff.Size
		fileInfo.Modified = diff.Modified
		switch diff.FileType {
		case persist.FileType_FILE:
			fileInfo.FileType = pfs.FileType_FILE_TYPE_REGULAR
		case persist.FileType_DIR:
			fileInfo.FileType = pfs.FileType_FILE_TYPE_DIR
		default:
			return nil, fmt.Errorf("unrecognized file type %d; this is likely a bug", diff.FileType)
		}
		fileInfo.CommitModified = &pfs.Commit{
			Repo: file.Commit.Repo,
			ID:   diff.CommitID,
		}
		fileInfos = append(fileInfos, fileInfo)
	}

	return fileInfos, nil
}

func (d *driver) DeleteFile(file *pfs.File, shard uint64, unsafe bool, handle string) error {
	fixPath(file)

	commit, err := d.getCommitByAmbiguousID(file.Commit.Repo.Name, file.Commit.ID)
	if err != nil {
		return err
	}

	query, err := d.getIntervalQueryFromCommitRange(nil, file.Commit)
	if err != nil {
		return err
	}

	repo := commit.Repo
	commitID := commit.ID
	prefix := file.Path

	// Get all files under the directory, ordered by path.
	cursor, err := foldDiffs(query.Map(func(clocks gorethink.Term) interface{} {
		return DiffPrefixIndex.Key(repo, prefix, clocks)
	}).EqJoin(func(x gorethink.Term) gorethink.Term {
		// no-op
		return x
	}, d.getTerm(diffTable), gorethink.EqJoinOpts{
		Index: DiffPrefixIndex.GetName(),
	}).Field("right").Group("Path")).Ungroup().Field("reduction").Filter(func(diff gorethink.Term) gorethink.Term {
		return diff.Field("FileType").Ne(persist.FileType_NONE)
	}).Field("Path").Run(d.dbClient)
	if err != nil {
		return err
	}

	var paths []string
	if err := cursor.All(&paths); err != nil {
		return err
	}
	paths = append(paths, prefix)

	var diffs []*persist.Diff
	for _, path := range paths {
		diffs = append(diffs, &persist.Diff{
			ID:           getDiffID(commitID, path),
			CommitID:     commitID,
			Repo:         repo,
			Path:         path,
			BlockRefs:    nil,
			Delete:       true,
			Size:         0,
			BranchClocks: commit.BranchClocks,
			FileType:     persist.FileType_NONE,
		})
	}

	// TODO: ideally we want to insert the documents ordered by their path,
	// where we insert the leaves first all the way to the root.  That way
	// we ensure the consistency of the file system: it's ok if we've removed
	// "/foo/bar" but not "/foo", but it's problematic if we've removed "/foo"
	// but not "/foo/bar"
	_, err = d.getTerm(diffTable).Insert(diffs, gorethink.InsertOpts{
		Conflict: "replace",
	}).RunWrite(d.dbClient)

	return err
}

func (d *driver) DeleteAll(shards map[uint64]bool) error {
	return nil
}

func (d *driver) AddShard(shard uint64) error {
	return nil
}

func (d *driver) DeleteShard(shard uint64) error {
	return nil
}

func (d *driver) Dump() {
}

func (d *driver) insertMessage(table Table, message proto.Message) error {
	_, err := d.getTerm(table).Insert(message).RunWrite(d.dbClient)
	return err
}

func (d *driver) updateMessage(table Table, message proto.Message) error {
	_, err := d.getTerm(table).Insert(message, gorethink.InsertOpts{Conflict: "update"}).RunWrite(d.dbClient)
	return err
}

func (d *driver) getMessageByPrimaryKey(table Table, key interface{}, message proto.Message) error {
	cursor, err := d.getTerm(table).Get(key).Run(d.dbClient)
	if err != nil {
		return err
	}
	err = cursor.One(message)
	if err == gorethink.ErrEmptyResult {
		return fmt.Errorf("%v not found in table %v", key, table)
	}
	return err
}

func (d *driver) getMessageByIndex(table Table, index Index, key interface{}, message proto.Message) error {
	cursor, err := d.getTerm(table).GetAllByIndex(index.GetName(), key).Run(d.dbClient)
	if err != nil {
		return err
	}
	err = cursor.One(message)
	if err == gorethink.ErrEmptyResult {
		return fmt.Errorf("%v not found in index %v of table %v", key, index, table)
	}
	return err
}

// betweenIndex returns a cursor that will return all documents in between two
// values on an index.
// rightBound specifies whether maxVal is included in the range.  Default is false.
func (d *driver) betweenIndex(table Table, index interface{}, minVal interface{}, maxVal interface{}, reverse bool, opts ...gorethink.BetweenOpts) gorethink.Term {
	if reverse {
		index = gorethink.Desc(index)
	}

	return d.getTerm(table).OrderBy(gorethink.OrderByOpts{
		Index: index,
	}).Between(minVal, maxVal, opts...)
}

func (d *driver) deleteMessageByPrimaryKey(table Table, key interface{}) error {
	_, err := d.getTerm(table).Get(key).Delete().RunWrite(d.dbClient)
	return err
}

// OBSOLETE
// Under the new scheme, the parent ID of a commit is self evident:
// The parent of foo/3 is foo/2, for instance.
func (d *driver) getIDOfParentCommit(repo string, commitID string) (string, error) {
	commit, err := d.getCommitByAmbiguousID(repo, commitID)
	if err != nil {
		return "", err
	}
	onlyBranch := commit.BranchClocks[0]
	numClocks := len(onlyBranch.Clocks)
	clock := onlyBranch.Clocks[numClocks-1]
	if clock.Clock == 0 {
		if numClocks < 2 {
			return "", nil
		}
		clock = onlyBranch.Clocks[numClocks-2]
	} else {
		clock.Clock -= 1
	}

	parentCommit := &persist.Commit{}
	if err := d.getMessageByIndex(commitTable, CommitBranchIndex, CommitBranchIndex.Key(commit.Repo, clock.Branch, clock.Clock), parentCommit); err != nil {
		return "", err
	}
	return parentCommit.ID, nil
}

// getCommitByAmbiguousID accepts a repo name and an ID, and returns a Commit object.
// The ID can be of 3 forms:
// 1. Database primary key: we are only supporting this case to maintain compatibility
// of the existing tests.  We will remove support for this case eventually.  OBSOLETE
// 2. branch/clock: like "master/3"
// 3. branch: like "master".  This would represent the head of the branch.
func (d *driver) getCommitByAmbiguousID(repo string, commitID string) (commit *persist.Commit, err error) {
	alias, err := parseClock(commitID)

	commit = &persist.Commit{}
	if err != nil {
		// We see if the commitID is a branch name
		if err := d.getHeadOfBranch(repo, commitID, commit); err != nil {
			if err != gorethink.ErrEmptyResult {
				return nil, err
			}

			// If the commit ID is not a branch name, we see if it's a database key
			cursor, err := d.getTerm(commitTable).Get(commitID).Run(d.dbClient)
			if err != nil {
				return nil, err
			}

			if err := cursor.One(commit); err != nil {
				return nil, err
			}
		}
	} else {
		// If we can't parse
		if err := d.getMessageByIndex(commitTable, CommitBranchIndex, CommitBranchIndex.Key(repo, alias.Branch, alias.Clock), commit); err != nil {
			return nil, err
		}
	}
	return commit, nil
}

func (d *driver) updateCommitWithAmbiguousID(repo string, commitID string, values map[string]interface{}) (err error) {
	alias, err := parseClock(commitID)
	if err != nil {
		_, err = d.getTerm(commitTable).Get(commitID).Update(values).RunWrite(d.dbClient)
	} else {
		_, err = d.getTerm(commitTable).GetAllByIndex(CommitBranchIndex.GetName(), CommitBranchIndex.Key(repo, alias.Branch, alias.Clock)).Update(values).RunWrite(d.dbClient)
	}
	return err
}