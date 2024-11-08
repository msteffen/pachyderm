package pfsdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pachyderm/pachyderm/v2/src/internal/authdb"
	"github.com/pachyderm/pachyderm/v2/src/internal/errutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/log"
	"github.com/pachyderm/pachyderm/v2/src/internal/pgjsontypes"
	"go.uber.org/zap"

	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pachyderm/pachyderm/v2/src/internal/collection"
	"github.com/pachyderm/pachyderm/v2/src/internal/dbutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/pachsql"
	"github.com/pachyderm/pachyderm/v2/src/internal/pbutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/randutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/stream"
	"github.com/pachyderm/pachyderm/v2/src/internal/watch/postgres"
	"github.com/pachyderm/pachyderm/v2/src/pfs"
)

const (
	MaxSearchDepth = 1000

	// CommitsChannelName is used to watch events for the commits table.
	CommitsChannelName     = "pfs_commits"
	CommitsRepoChannelName = "pfs_commits_repo_"
	CommitChannelName      = "pfs_commits_"
	createCommit           = `
		WITH repo_row_id AS (SELECT id from pfs.repos WHERE name=$1 AND type=$2 AND project_id=(SELECT id from core.projects WHERE name=$3))
		INSERT INTO pfs.commits
    	(commit_id,
    	 commit_set_id,
    	 repo_id,
    	 branch_id,
    	 description,
    	 origin,
    	 start_time,
    	 finishing_time,
    	 finished_time,
    	 compacting_time_s,
    	 validating_time_s,
    	 size,
    	 error,
    	 metadata,
    	 created_by)
		VALUES
		($4, $5,
		 (SELECT id from repo_row_id),
		 (SELECT id from pfs.branches WHERE name=$6 AND repo_id=(SELECT id from repo_row_id)),
		 $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		RETURNING int_id;`
	commitFields = `
		commit.int_id,
		commit.commit_id,
		commit.commit_set_id,
		commit.branch_id,
		commit.origin,
		commit.description,
		commit.start_time,
		commit.finishing_time,
		commit.finished_time,
		commit.compacting_time_s,
		commit.validating_time_s,
		commit.error,
		commit.size,
		commit.created_at,
		commit.updated_at,
		commit.metadata,
		commit.created_by,
		commit.repo_id AS "repo.id",
		repo.name AS "repo.name",
		repo.type AS "repo.type",
		project.name AS "repo.project.name",
		branch.name as branch_name`
	commitFieldsGroupBy = `
		commit.int_id,
		commit.commit_id,
		commit.commit_set_id,
		commit.branch_id,
		commit.origin,
		commit.description,
		commit.start_time,
		commit.finishing_time,
		commit.finished_time,
		commit.compacting_time_s,
		commit.validating_time_s,
		commit.error,
		commit.size,
		commit.created_at,
		commit.updated_at,
		commit.metadata,
		commit.created_by,
		commit.repo_id,
		repo.name,
		repo.type,
		project.name,
		branch.name`
	getCommit = "SELECT DISTINCT " + commitFields +
		`
		FROM pfs.commits commit
		JOIN pfs.repos repo ON commit.repo_id = repo.id
		JOIN core.projects project ON repo.project_id = project.id
		LEFT JOIN pfs.branches branch ON commit.branch_id = branch.id`
	getParentCommit = getCommit + `
		JOIN pfs.commit_ancestry ancestry ON ancestry.parent = commit.int_id`
	getChildCommit = getCommit + `
		JOIN pfs.commit_ancestry ancestry ON ancestry.child = commit.int_id`
	commitsPageSize = 1000
)

// CommitNotFoundError is returned by GetCommitInfo() when a commit is not found in postgres.
type CommitNotFoundError struct {
	RowID    CommitID
	CommitID string
}

func (err *CommitNotFoundError) Error() string {
	return fmt.Sprintf("commit (int_id=%d, commit_id=%s) not found", err.RowID, err.CommitID)
}

func (err *CommitNotFoundError) GRPCStatus() *status.Status {
	return status.New(codes.NotFound, err.Error())
}

// ParentCommitNotFoundError is returned when a commit's parent is not found in postgres.
type ParentCommitNotFoundError struct {
	ChildRowID    CommitID
	ChildCommitID string
}

func IsParentCommitNotFound(err error) bool {
	return dbutil.IsNotNullViolation(err, "parent")
}

func (err *ParentCommitNotFoundError) Error() string {
	return fmt.Sprintf("parent commit of commit (int_id=%d, commit_id=%s) not found", err.ChildRowID, err.ChildCommitID)
}

func (err *ParentCommitNotFoundError) GRPCStatus() *status.Status {
	return status.New(codes.NotFound, err.Error())
}

// ChildCommitNotFoundError is returned when a commit's child is not found in postgres.
type ChildCommitNotFoundError struct {
	Repo           string
	ParentRowID    CommitID
	ParentCommitID string
}

func IsChildCommitNotFound(err error) bool {
	return dbutil.IsNotNullViolation(err, "child")
}

func (err *ChildCommitNotFoundError) Error() string {
	return fmt.Sprintf("parent commit of commit (int_id=%d, commit_key=%s) not found", err.ParentRowID, err.ParentCommitID)
}

func (err *ChildCommitNotFoundError) GRPCStatus() *status.Status {
	return status.New(codes.NotFound, err.Error())
}

// CommitMissingInfoError is returned when a commitInfo is missing a field.
type CommitMissingInfoError struct {
	Field string
}

func (err *CommitMissingInfoError) Error() string {
	return fmt.Sprintf("commitInfo.%s is missing/nil", err.Field)
}

func (err *CommitMissingInfoError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, err.Error())
}

// CommitAlreadyExistsError is returned when a commit with the same name already exists in postgres.
type CommitAlreadyExistsError struct {
	CommitID string
}

// Error satisfies the error interface.
func (err *CommitAlreadyExistsError) Error() string {
	return fmt.Sprintf("commit %s already exists", err.CommitID)
}

func (err *CommitAlreadyExistsError) GRPCStatus() *status.Status {
	return status.New(codes.AlreadyExists, err.Error())
}

// AncestryOpt allows users to create commitInfos and skip creating the ancestry information.
// This allows a user to create the commits in an arbitrary order, then create their ancestry later.
type AncestryOpt struct {
	SkipChildren bool
	SkipParent   bool
}

// CommitsInRepoChannel returns the name of the channel that is notified when commits in repo 'repoID' are created, updated, or deleted
func CommitsInRepoChannel(repoID RepoID) string {
	return fmt.Sprintf("%s%d", CommitsRepoChannelName, repoID)
}

// CreateCommit creates an entry in the pfs.commits table. If the commit has a parent or children,
// it will attempt to create entries in the pfs.commit_ancestry table unless options are provided to skip
// ancestry creation.
func CreateCommit(ctx context.Context, tx *pachsql.Tx, commitInfo *pfs.CommitInfo, opts ...AncestryOpt) (CommitID, error) {
	if err := validateCommitInfo(commitInfo); err != nil {
		return 0, err
	}
	commit, err := GetCommitByKey(ctx, tx, commitInfo.Commit)
	if err == nil {
		return 0, &CommitAlreadyExistsError{CommitID: commit.CommitInfo.Commit.Key()}
	}
	if errors.As(err, new(*ProjectNotFoundError)) || errors.As(err, new(*RepoNotFoundError)) {
		return 0, err
	}
	if !errors.As(err, new(*CommitNotFoundError)) {
		return 0, err
	}
	opt := AncestryOpt{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	branchName := sql.NullString{String: "", Valid: false}
	if commitInfo.Commit.Branch != nil {
		branchName = sql.NullString{String: commitInfo.Commit.Branch.Name, Valid: true}
	}
	var createdBy sql.NullString
	if creator := commitInfo.CreatedBy; creator != "" {
		createdBy.String = creator
		createdBy.Valid = true
		if err := authdb.EnsurePrincipal(ctx, tx, creator); err != nil {
			return 0, errors.Wrapf(err, "ensure principal %v", creator)
		}
	}
	insert := CommitRow{
		CommitID:    CommitKey(commitInfo.Commit),
		CommitSetID: commitInfo.Commit.Id,
		Repo: RepoRow{
			Name: commitInfo.Commit.Repo.Name,
			Type: commitInfo.Commit.Repo.Type,
			Project: ProjectRow{
				Name: commitInfo.Commit.Repo.Project.Name,
			},
		},
		BranchName:     branchName,
		Origin:         commitInfo.Origin.Kind.String(),
		StartTime:      pbutil.SanitizeTimestampPb(commitInfo.Started),
		FinishingTime:  pbutil.SanitizeTimestampPb(commitInfo.Finishing),
		FinishedTime:   pbutil.SanitizeTimestampPb(commitInfo.Finished),
		Description:    commitInfo.Description,
		CompactingTime: pbutil.DurationPbToBigInt(commitInfo.Details.CompactingTime),
		ValidatingTime: pbutil.DurationPbToBigInt(commitInfo.Details.ValidatingTime),
		Size:           commitInfo.Details.SizeBytes,
		Error:          commitInfo.Error,
		Metadata:       pgjsontypes.StringMap{Data: commitInfo.Metadata},
		CreatedBy:      createdBy,
	}
	// It would be nice to use a named query here, but sadly there is no NamedQueryRowContext. Additionally,
	// we run into errors when using named statements: (named statement already exists).

	row := tx.QueryRowxContext(ctx, createCommit, insert.Repo.Name, insert.Repo.Type, insert.Repo.Project.Name,
		insert.CommitID, insert.CommitSetID, insert.BranchName, insert.Description, insert.Origin, insert.StartTime, insert.FinishingTime,
		insert.FinishedTime, insert.CompactingTime, insert.ValidatingTime, insert.Size, insert.Error, insert.Metadata, insert.CreatedBy)
	lastInsertId := 0
	if err := row.Scan(&lastInsertId); err != nil {
		return 0, errors.Wrap(err, "scanning id from create commitInfo")
	}
	if commitInfo.ParentCommit != nil && !opt.SkipParent {
		if err := CreateCommitParent(ctx, tx, commitInfo.ParentCommit, CommitID(lastInsertId)); err != nil {
			return 0, errors.Wrap(err, "linking parent")
		}
	}
	if len(commitInfo.ChildCommits) != 0 && !opt.SkipChildren {
		if err := CreateCommitChildren(ctx, tx, CommitID(lastInsertId), commitInfo.ChildCommits); err != nil {
			return 0, errors.Wrap(err, "linking children")
		}
	}
	return CommitID(lastInsertId), nil
}

// CreateCommitParent inserts a single ancestry relationship where the child is known and parent must be derived.
func CreateCommitParent(ctx context.Context, tx *pachsql.Tx, parentCommit *pfs.Commit, childCommit CommitID) error {
	ancestryQuery := `
		INSERT INTO pfs.commit_ancestry
		(parent, child)
		VALUES ((SELECT int_id FROM pfs.commits WHERE commit_id=$1), $2)
		ON CONFLICT DO NOTHING;
	`
	_, err := tx.ExecContext(ctx, ancestryQuery, CommitKey(parentCommit), childCommit)
	if err != nil {
		if IsParentCommitNotFound(err) {
			return &ParentCommitNotFoundError{ChildRowID: childCommit}
		}
		return errors.Wrap(err, "putting commit parent")
	}
	return nil
}

// CreateCommitAncestries inserts ancestry relationships where the ids of both parent and children are known.
func CreateCommitAncestries(ctx context.Context, tx *pachsql.Tx, parentCommit CommitID, childrenCommits []CommitID) error {
	ancestryQueryTemplate := `
		INSERT INTO pfs.commit_ancestry
		(parent, child)
		VALUES %s
		ON CONFLICT DO NOTHING;
	`
	childValuesTemplate := `($1, $%d)`
	params := []any{parentCommit}
	queryVarNum := 2
	values := make([]string, 0)
	for _, child := range childrenCommits {
		values = append(values, fmt.Sprintf(childValuesTemplate, queryVarNum))
		params = append(params, child)
		queryVarNum++
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(ancestryQueryTemplate, strings.Join(values, ",")),
		params...)
	if err != nil {
		if IsChildCommitNotFound(err) {
			return &ChildCommitNotFoundError{ParentRowID: parentCommit}
		}
		return errors.Wrap(err, "putting commit children")
	}
	return nil
}

// CreateCommitChildren inserts ancestry relationships using a single query for all of the children.
func CreateCommitChildren(ctx context.Context, tx *pachsql.Tx, parentCommit CommitID, childCommits []*pfs.Commit) error {
	ancestryQueryTemplate := `
		INSERT INTO pfs.commit_ancestry
		(parent, child)
		VALUES %s
		ON CONFLICT DO NOTHING;
	`
	childValuesTemplate := `($1, (SELECT int_id FROM pfs.commits WHERE commit_id=$%d))`
	params := []any{parentCommit}
	queryVarNum := 2
	values := make([]string, 0)
	for _, child := range childCommits {
		values = append(values, fmt.Sprintf(childValuesTemplate, queryVarNum))
		params = append(params, CommitKey(child))
		queryVarNum++
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(ancestryQueryTemplate, strings.Join(values, ",")),
		params...)
	if err != nil {
		if IsChildCommitNotFound(err) {
			return &ChildCommitNotFoundError{ParentRowID: parentCommit}
		}
		return errors.Wrap(err, "putting commit children")
	}
	return nil
}

// DeleteCommit deletes an entry in the pfs.commits table. It also repoints the references in the commit_ancestry table.
// The caller is responsible for updating branchesg.
func DeleteCommit(ctx context.Context, tx *pachsql.Tx, commit *pfs.Commit) error {
	if commit == nil {
		return &CommitMissingInfoError{Field: "Commit"}
	}
	id, err := GetCommitID(ctx, tx, commit)
	if err != nil {
		return errors.Wrap(err, "getting commit ID to delete")
	}
	parent, children, err := getCommitRelativeRows(ctx, tx, id)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("getting commit relatives for id=%d", id))
	}
	// delete commit.parent -> commit and commit -> commit.children if they exist.
	if parent != nil || children != nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM pfs.commit_ancestry WHERE parent=$1 OR child=$1;", id)
		if err != nil {
			return errors.Wrap(err, "delete commit ancestry")
		}
	}
	// repoint commit.parent -> commit.children
	if parent != nil && children != nil {
		childrenIDs := make([]CommitID, 0)
		for _, child := range children {
			childrenIDs = append(childrenIDs, child.ID)
		}
		if err := CreateCommitAncestries(ctx, tx, parent.ID, childrenIDs); err != nil {
			return errors.Wrap(err, fmt.Sprintf("repointing id=%d at %v", parent.ID, childrenIDs))
		}
	}
	// delete commit.
	result, err := tx.ExecContext(ctx, "DELETE FROM pfs.commits WHERE int_id=$1;", id)
	if err != nil {
		return errors.Wrap(err, "delete commit")
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "could not get affected rows")
	}
	if rowsAffected == 0 {
		return &CommitNotFoundError{RowID: id}
	}
	return nil
}

// GetCommitID returns the int_id of a commit in postgres.
func GetCommitID(ctx context.Context, tx *pachsql.Tx, commit *pfs.Commit) (CommitID, error) {
	if commit == nil {
		return 0, &CommitMissingInfoError{Field: "Commit"}
	}
	if commit.Repo == nil {
		return 0, &CommitMissingInfoError{Field: "Repo"}
	}
	row, err := getCommitRowByCommitKey(ctx, tx, commit)
	if err != nil {
		return 0, errors.Wrap(err, "get commit by commit key")
	}
	return row.ID, nil
}

// GetCommitInfo returns the commitInfo where int_id=id.
func GetCommitInfo(ctx context.Context, tx *pachsql.Tx, id CommitID) (*pfs.CommitInfo, error) {
	c, err := GetCommit(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return c.CommitInfo, nil
}

// GetCommit returns the *pfsdb.Commit where int_id=id.
func GetCommit(ctx context.Context, tx *pachsql.Tx, id CommitID) (*Commit, error) {
	return getCommitInternal(ctx, tx, id)
}

func getCommitInternal(ctx context.Context, extCtx sqlx.ExtContext, id CommitID) (*Commit, error) {
	row, err := getCommitRow(ctx, extCtx, id)
	if err != nil {
		return nil, errors.Wrap(err, "get commit info from row")
	}
	commitInfo, relatedCommitIDs, err := getCommitFromCommitRow(ctx, extCtx, row)
	if err != nil {
		return nil, errors.Wrap(err, "get commit info from row")
	}
	return &Commit{
		ID:               id,
		CommitInfo:       commitInfo,
		relatedCommitIDs: *relatedCommitIDs,
	}, err
}

func GetCommitByKey(ctx context.Context, tx *pachsql.Tx, commit *pfs.Commit) (*Commit, error) {
	row, err := getCommitRowByCommitKey(ctx, tx, commit)
	if err != nil {
		return nil, errors.Wrap(err, "get commit by commit key")
	}
	commitInfo, relatedCommitIDs, err := getCommitFromCommitRow(ctx, tx, row)
	if err != nil {
		return nil, errors.Wrap(err, "get commit info from row")
	}
	return &Commit{
		CommitInfo:       commitInfo,
		ID:               row.ID,
		relatedCommitIDs: *relatedCommitIDs,
	}, nil
}

// GetCommitInfoByKey is like GetCommitInfo but derives the int_id on behalf of the caller.
func GetCommitInfoByKey(ctx context.Context, tx *pachsql.Tx, commit *pfs.Commit) (*pfs.CommitInfo, error) {
	pair, err := GetCommitByKey(ctx, tx, commit)
	if err != nil {
		return nil, err
	}
	return pair.CommitInfo, nil
}

// GetCommitParent uses the pfs.commit_ancestry and pfs.commits tables to retrieve a commit given an int_id of
// one of its children.
func GetCommitParent(ctx context.Context, tx *pachsql.Tx, childCommit CommitID) (*pfs.Commit, error) {
	commit, _, err := getCommitParent(ctx, tx, childCommit)
	return commit, err
}

// GetCommitParent uses the pfs.commit_ancestry and pfs.commits tables to retrieve a commit given an int_id of
// one of its children.
func getCommitParent(ctx context.Context, extCtx sqlx.ExtContext, childCommit CommitID) (*pfs.Commit, *CommitRow, error) {
	row, err := getCommitParentRow(ctx, extCtx, childCommit)
	if err != nil {
		return nil, nil, errors.Wrap(err, "getting parent commit row")
	}
	parentCommitInfo := parseCommitInfoFromRow(row)
	return parentCommitInfo.Commit, row, nil
}

// GetCommitChildren uses the pfs.commit_ancestry and pfs.commits tables to retrieve commits of all
// of the children given an int_id of the parent.
func GetCommitChildren(ctx context.Context, tx *pachsql.Tx, parentCommit CommitID) ([]*pfs.Commit, error) {
	children, _, err := getCommitChildren(ctx, tx, parentCommit)
	return children, err
}

func getCommitChildren(ctx context.Context, extCtx sqlx.ExtContext, parentCommit CommitID) ([]*pfs.Commit, []*CommitRow, error) {
	children := make([]*pfs.Commit, 0)
	rows, err := extCtx.QueryxContext(ctx, fmt.Sprintf("%s WHERE ancestry.parent=$1", getChildCommit), parentCommit)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, &ChildCommitNotFoundError{ParentRowID: parentCommit}
		}
		return nil, nil, errors.Wrap(err, "getting commit children")
	}
	var commitRows []*CommitRow
	for rows.Next() {
		row := &CommitRow{}
		if err := rows.StructScan(row); err != nil {
			return nil, nil, errors.Wrap(err, "scanning commit row for child")
		}
		childCommitInfo := parseCommitInfoFromRow(row)
		children = append(children, childCommitInfo.Commit)
		commitRows = append(commitRows, row)
	}
	if len(children) == 0 { // QueryxContext does not return an error when the query returns 0 rows.
		return nil, nil, &ChildCommitNotFoundError{ParentRowID: parentCommit}
	}
	return children, commitRows, nil
}

// GetCommitAncestry returns a map of child CommitID values to parent CommitIDs including the startId up to maxDepth.
// maxDepth cannot exceed MaxSearchDepth.
func GetCommitAncestry(ctx context.Context, extCtx sqlx.ExtContext, startId CommitID, maxDepth uint) (map[CommitID]CommitID, error) {
	ancestry := make(map[CommitID]CommitID)
	if err := ForEachCommitAncestor(ctx, extCtx, startId, maxDepth, func(parentId, childId CommitID) error {
		ancestry[childId] = parentId
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "get commit ancestry")
	}
	return ancestry, nil
}

// ForEachCommitAncestor queries postgres for ancestors of startId up to the maxDepth. cb() is called for each ancestor.
// maxDepth is optional, but cannot exceed MaxSearchDepth. The caller may gracefully terminate iteration early by
// returning errutil.ErrBreak in cb().
func ForEachCommitAncestor(ctx context.Context, extCtx sqlx.ExtContext, startId CommitID, maxDepth uint, cb func(parentId, childId CommitID) error) error {
	if maxDepth == 0 || maxDepth > MaxSearchDepth {
		maxDepth = MaxSearchDepth
	}
	query := `
	WITH RECURSIVE ancestry AS (
		SELECT parent, child, 1 as depth FROM pfs.commit_ancestry WHERE child = $1
		UNION
		SELECT ca.parent, ca.child, depth+1 FROM pfs.commit_ancestry ca
		JOIN ancestry a ON ca.child = a.parent WHERE depth < $2
	)
	SELECT a.parent, a.child, depth
	FROM ancestry a;`
	rows, err := extCtx.QueryContext(ctx, query, startId, maxDepth)
	if err != nil {
		return errors.Wrap(err, "get commit ancestry")
	}
	defer func() {
		err := rows.Close()
		if err != nil {
			log.Error(ctx, "closing rows", zap.Error(err))
		}
	}()
	for rows.Next() {
		var parent, child CommitID
		var depth uint
		if err := rows.Scan(&parent, &child, &depth); err != nil {
			return errors.Wrap(err, "scanning parent and child row")
		}
		if err := cb(parent, child); err != nil {
			return errors.Wrap(err, "calling cb() on parent and child")
		}
	}
	if err := rows.Err(); err != nil {
		return errors.Wrap(err, "iterating over commit ancestry")
	}
	return nil
}

// forEachCommitAncestorUntilRoot calls ForEachCommitAncestor continuously until the root is encountered.
func forEachCommitAncestorUntilRoot(ctx context.Context, tx *pachsql.Tx, startId CommitID, cb func(parentId, childId CommitID) error) error {
	commitPtr := startId
	earliest := commitPtr
	for {
		if err := ForEachCommitAncestor(ctx, tx, commitPtr, MaxSearchDepth, func(parentId, childId CommitID) error {
			earliest = parentId
			if err := cb(parentId, childId); err != nil {
				return err
			}
			return nil
		}); err != nil {
			if errors.Is(err, errutil.ErrBreak) {
				return nil
			}
			return errors.Wrap(err, "for each commit ancestor in batches")
		}
		if earliest == commitPtr { // root was found.
			return nil
		}
		commitPtr = earliest
	}
}

// UpdateCommitBranch updates a commit's branch related fields only.
// This is a separate function to make it easier to audit updates to a commit's branch for the removal of the
// branch related fields in the future.
func UpdateCommitBranch(ctx context.Context, tx *pachsql.Tx, commitID CommitID, branchID BranchID) error {
	query := `UPDATE pfs.commits SET branch_id=:branch_id WHERE int_id=:int_id;`
	commitRow := &CommitRow{
		ID: commitID,
		BranchID: sql.NullInt64{
			Valid: true,
			Int64: int64(branchID),
		},
	}
	return errors.Wrap(update(ctx, tx, query, commitRow), "update commit branch")
}

func FinishingCommit(ctx context.Context, tx *pachsql.Tx, commitID CommitID, finishingTime *timestamppb.Timestamp, error string) error {
	query := `UPDATE pfs.commits SET finishing_time=:finishing_time, error=:error WHERE int_id=:int_id;`
	commitRow := &CommitRow{
		ID:            commitID,
		FinishingTime: pbutil.SanitizeTimestampPb(finishingTime),
		Error:         error,
	}
	return errors.Wrap(update(ctx, tx, query, commitRow), "finishing commit")
}

func FinishCommit(ctx context.Context, tx *pachsql.Tx, commitID CommitID, finishedTime *timestamppb.Timestamp, error string, details *pfs.CommitInfo_Details) error {
	query := `UPDATE pfs.commits SET 
			finished_time=:finished_time, 
			error=:error, 
			compacting_time_s=:compacting_time_s, 
			validating_time_s=:validating_time_s, 
			size=:size 
		WHERE int_id=:int_id;`
	commitRow := &CommitRow{
		ID:             commitID,
		FinishedTime:   pbutil.SanitizeTimestampPb(finishedTime),
		CompactingTime: pbutil.DurationPbToBigInt(details.CompactingTime),
		ValidatingTime: pbutil.DurationPbToBigInt(details.ValidatingTime),
		Size:           details.SizeBytes,
		Error:          error,
	}
	return errors.Wrap(update(ctx, tx, query, commitRow), "finish commit")
}

func UpdateCommitMetadata(ctx context.Context, tx *pachsql.Tx, commitID CommitID, metadata map[string]string) error {
	query := `UPDATE pfs.commits SET metadata=:metadata WHERE int_id=:int_id;`
	commitRow := &CommitRow{
		ID:       commitID,
		Metadata: pgjsontypes.StringMap{Data: metadata},
	}
	return errors.Wrap(update(ctx, tx, query, commitRow), "update commit metadata")
}

func UpdateDescription(ctx context.Context, tx *pachsql.Tx, commitID CommitID, description string) error {
	query := `UPDATE pfs.commits SET description=:description WHERE int_id=:int_id;`
	commitRow := &CommitRow{
		ID:          commitID,
		Description: description,
	}
	return errors.Wrap(update(ctx, tx, query, commitRow), "update description")
}

func update(ctx context.Context, tx *pachsql.Tx, query string, row *CommitRow) error {
	res, err := tx.NamedExecContext(ctx, query, row)
	if err != nil {
		return errors.Wrap(err, "update commit")
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "update commit: rows affected")
	}
	if rowsAffected == 0 {
		return &CommitNotFoundError{RowID: row.ID}
	}
	return nil
}

// validateCommitInfo returns an error if the commit is not valid and has side effects of instantiating details
// if they are nil.
func validateCommitInfo(commitInfo *pfs.CommitInfo) error {
	if commitInfo.Commit == nil {
		return &CommitMissingInfoError{Field: "Commit"}
	}
	if commitInfo.Commit.Repo == nil {
		return &CommitMissingInfoError{Field: "Repo"}
	}
	if commitInfo.Origin == nil {
		return &CommitMissingInfoError{Field: "Origin"}
	}
	if commitInfo.Details == nil { // stub in an empty details struct to avoid panics.
		commitInfo.Details = &pfs.CommitInfo_Details{}
	}
	switch commitInfo.Origin.Kind {
	case pfs.OriginKind_ORIGIN_KIND_UNKNOWN, pfs.OriginKind_USER, pfs.OriginKind_AUTO, pfs.OriginKind_FSCK:
		break
	default:
		return errors.New(fmt.Sprintf("invalid origin: %v", commitInfo.Origin.Kind))
	}
	return nil
}

func getCommitInfoFromCommitRow(ctx context.Context, extCtx sqlx.ExtContext, row *CommitRow) (*pfs.CommitInfo, error) {
	info, _, err := getCommitFromCommitRow(ctx, extCtx, row)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// getCommitFromCommitRow returns commits in different formats as needed by a caller. A common format is the 'CommitInfo' format.
// additionally, it returns the related commit IDs. The goal is to eventually not return the CommitInfo and just return the related
// IDs. Both types are returned to allow for callers to have more flexibility.
func getCommitFromCommitRow(ctx context.Context, extCtx sqlx.ExtContext, row *CommitRow) (*pfs.CommitInfo, *relatedCommitIDs, error) {
	var (
		err                       error
		parent                    *CommitRow
		parentID                  CommitID
		children                  []*CommitRow
		childIDs, provIDs, subIDs []CommitID
	)
	commitInfo := parseCommitInfoFromRow(row)
	commitInfo.ParentCommit, commitInfo.ChildCommits, parent, children, err = getCommitRelativesAndRows(ctx, extCtx, row.ID)
	if err != nil {
		return nil, nil, errors.Wrap(err, "get commit relatives")
	}
	if parent != nil {
		parentID = parent.ID
	}
	for _, commit := range children {
		childIDs = append(childIDs, commit.ID)
	}
	provenantCommits, err := getProvenantCommitRows(ctx, extCtx, row.ID, WithMaxDepth(1))
	if err != nil {
		return nil, nil, errors.Wrap(err, "get commit provenance")
	}
	for _, commit := range provenantCommits {
		commitInfo.DirectProvenance = append(commitInfo.DirectProvenance, commit.Pb())
		provIDs = append(provIDs, commit.ID)
	}
	subvenantCommits, err := getSubvenantCommitRows(ctx, extCtx, row.ID, WithMaxDepth(1))
	if err != nil {
		return nil, nil, errors.Wrap(err, "get commit subvenance")
	}
	for _, commit := range subvenantCommits {
		commitInfo.DirectSubvenance = append(commitInfo.DirectSubvenance, commit.Pb())
		subIDs = append(subIDs, commit.ID)
	}
	relatedCommitIDs := &relatedCommitIDs{
		ParentID:           parentID,
		ChildrenIDs:        childIDs,
		DirectProvenantIDs: provIDs,
		DirectSubvenantIDs: subIDs,
	}
	return commitInfo, relatedCommitIDs, err
}

// getCommitRelativesAndRows returns parents and children in two different formats. Eventually, the goal is to only return
// the rows rather than the pfs.Commit structures.
func getCommitRelativesAndRows(
	ctx context.Context,
	extCtx sqlx.ExtContext,
	commitID CommitID) (
	parentCommit *pfs.Commit,
	childCommits []*pfs.Commit,
	parentRow *CommitRow,
	childRows []*CommitRow,
	err error) {
	parentCommit, parentRow, err = getCommitParent(ctx, extCtx, commitID)
	if err != nil && !errors.As(err, &ParentCommitNotFoundError{ChildRowID: commitID}) {
		return nil, nil, nil, nil, errors.Wrap(err, "getting parent commit")
		// if parent is missing, assume commit is root of a repo.
	}
	childCommits, childRows, err = getCommitChildren(ctx, extCtx, commitID)
	if err != nil && !errors.As(err, &ChildCommitNotFoundError{ParentRowID: commitID}) {
		return nil, nil, nil, nil, errors.Wrap(err, "getting children commits")
		// if children is missing, assume commit is HEAD of some branch.
	}
	return parentCommit, childCommits, parentRow, childRows, nil
}

func getCommitParentRow(ctx context.Context, extCtx sqlx.ExtContext, childCommit CommitID) (*CommitRow, error) {
	row := &CommitRow{}
	if err := sqlx.GetContext(ctx, extCtx, row, fmt.Sprintf("%s WHERE ancestry.child=$1", getParentCommit), childCommit); err != nil {
		if err == sql.ErrNoRows {
			return nil, &ParentCommitNotFoundError{ChildRowID: childCommit}
		}
		return nil, errors.Wrap(err, "scanning commit row for parent")
	}
	return row, nil
}

func getCommitChildrenRows(ctx context.Context, tx *pachsql.Tx, parentCommit CommitID) ([]*CommitRow, error) {
	children := make([]*CommitRow, 0)
	rows, err := tx.QueryxContext(ctx, fmt.Sprintf("%s WHERE ancestry.parent=$1", getChildCommit), parentCommit)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &ChildCommitNotFoundError{ParentRowID: parentCommit}
		}
		return nil, errors.Wrap(err, "getting commit children rows")
	}
	for rows.Next() {
		row := &CommitRow{}
		if err := rows.StructScan(row); err != nil {
			return nil, errors.Wrap(err, "scanning commit row for child")
		}
		children = append(children, row)
	}
	if len(children) == 0 { // QueryxContext does not return an error when the query returns 0 rows.
		return nil, &ChildCommitNotFoundError{ParentRowID: parentCommit}
	}
	return children, nil
}

func getCommitRelativeRows(ctx context.Context, tx *pachsql.Tx, commitID CommitID) (*CommitRow, []*CommitRow, error) {
	commitParentRows, err := getCommitParentRow(ctx, tx, commitID)
	if err != nil && !errors.As(err, &ParentCommitNotFoundError{ChildRowID: commitID}) {
		return nil, nil, errors.Wrap(err, "getting parent commit")
		// if parent is missing, assume commit is root of a repo.
	}
	commitChildrenRows, err := getCommitChildrenRows(ctx, tx, commitID)
	if err != nil && !errors.As(err, &ChildCommitNotFoundError{ParentRowID: commitID}) {
		return nil, nil, errors.Wrap(err, "getting children commits")
		// if children is missing, assume commit is HEAD of some branch.
	}
	return commitParentRows, commitChildrenRows, nil
}

func getCommitRowByCommitKey(ctx context.Context, tx *pachsql.Tx, commit *pfs.Commit) (*CommitRow, error) {
	row := &CommitRow{}
	if commit == nil {
		return nil, &CommitMissingInfoError{Field: "Commit"}
	}
	id := CommitKey(commit)
	err := tx.QueryRowxContext(ctx, fmt.Sprintf("%s WHERE commit_id=$1", getCommit), id).StructScan(row)
	if err != nil {
		if err == sql.ErrNoRows {
			_, err := GetRepoByName(ctx, tx, commit.Repo.Project.Name, commit.Repo.Name, commit.Repo.Type)
			if err != nil {
				if errors.As(err, new(*RepoNotFoundError)) {
					return nil, errors.Join(err, &CommitNotFoundError{CommitID: id})
				}
				return nil, errors.Wrapf(err, "get repo for scan commit row with commit id %v", id)
			}
			return nil, &CommitNotFoundError{CommitID: id}
		}
		return nil, errors.Wrap(err, "scanning commit row")
	}
	return row, nil
}

func getCommitRow(ctx context.Context, extCtx sqlx.ExtContext, id CommitID) (*CommitRow, error) {
	if id == 0 {
		return nil, errors.New("invalid id: 0")
	}
	row := &CommitRow{}
	err := sqlx.GetContext(ctx, extCtx, row, fmt.Sprintf("%s WHERE int_id=$1", getCommit), id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &CommitNotFoundError{RowID: id}
		}
		return nil, errors.Wrap(err, "scanning commit row")
	}
	return row, nil
}

func parseCommitInfoFromRow(row *CommitRow) *pfs.CommitInfo {
	commitInfo := &pfs.CommitInfo{
		Commit:      row.Pb(),
		Origin:      &pfs.CommitOrigin{Kind: pfs.OriginKind(pfs.OriginKind_value[strings.ToUpper(row.Origin)])},
		Started:     pbutil.TimeToTimestamppb(row.StartTime),
		Finishing:   pbutil.TimeToTimestamppb(row.FinishingTime),
		Finished:    pbutil.TimeToTimestamppb(row.FinishedTime),
		Description: row.Description,
		Error:       row.Error,
		Metadata:    row.Metadata.Data,
		CreatedBy:   row.CreatedBy.String,
		Details: &pfs.CommitInfo_Details{
			CompactingTime: pbutil.BigIntToDurationpb(row.CompactingTime),
			ValidatingTime: pbutil.BigIntToDurationpb(row.ValidatingTime),
			SizeBytes:      row.Size,
		},
		CreatedAt: timestamppb.New(row.CreatedAt),
		UpdatedAt: timestamppb.New(row.UpdatedAt),
	}
	return commitInfo
}

type relatedCommitIDs struct {
	ChildrenIDs        []CommitID
	ParentID           CommitID
	DirectProvenantIDs []CommitID
	DirectSubvenantIDs []CommitID
}

// Commit wraps a *pfs.CommitInfo with a CommitID and an optional Revision.
// The Revision is set by a CommitIterator.
type Commit struct {
	ID CommitID
	*pfs.CommitInfo
	Revision int64
	relatedCommitIDs
}

// this dropped global variable instantiation forces the compiler to check whether CommitIterator implements stream.Iterator.
var _ stream.Iterator[Commit] = &CommitIterator{}

// commitColumn is used in the ForEachCommit filter and defines specific field names for type safety.
// This should hopefully prevent a library user from misconfiguring the filter.
type commitColumn string

var (
	CommitColumnID        = commitColumn("commit.int_id")
	CommitColumnSetID     = commitColumn("commit.commit_set_id")
	CommitColumnRepoID    = commitColumn("commit.repo_id")
	CommitColumnOrigin    = commitColumn("commit.origin")
	CommitColumnCreatedAt = commitColumn("commit.created_at")
	CommitColumnUpdatedAt = commitColumn("commit.updated_at")
)

type OrderByCommitColumn OrderByColumn[commitColumn]

type CommitIterator struct {
	paginator pageIterator[CommitRow]
	extCtx    sqlx.ExtContext
}

func (i *CommitIterator) Next(ctx context.Context, dst *Commit) error {
	if dst == nil {
		return errors.Errorf("dst CommitInfo cannot be nil")
	}
	commit, rev, err := i.paginator.next(ctx, i.extCtx)
	if err != nil {
		return err
	}
	commitInfo, err := getCommitInfoFromCommitRow(ctx, i.extCtx, commit)
	if err != nil {
		return err
	}
	dst.ID = commit.ID
	dst.CommitInfo = commitInfo
	dst.Revision = rev
	return nil
}

func NewCommitsIterator(ctx context.Context, extCtx sqlx.ExtContext, startPage, pageSize uint64, filter *pfs.Commit, orderBys ...OrderByCommitColumn) (*CommitIterator, error) {
	var conditions []string
	var values []any
	// Note that using ? as the bindvar is okay because we rebind it later.
	if filter != nil {
		if filter.Repo != nil && filter.Repo.Name != "" {
			conditions = append(conditions, "repo.name = ?")
			values = append(values, filter.Repo.Name)
		}
		if filter.Repo != nil && filter.Repo.Type != "" {
			conditions = append(conditions, "repo.type = ?")
			values = append(values, filter.Repo.Type)
		}
		if filter.Repo != nil && filter.Repo.Project != nil && filter.Repo.Project.Name != "" {
			conditions = append(conditions, "project.name = ?")
			values = append(values, filter.Repo.Project.Name)
		}
		if filter.Id != "" {
			conditions = append(conditions, "commit.commit_set_id = ?")
			values = append(values, filter.Id)
		}
		if filter.Branch != nil && filter.Branch.Name != "" {
			conditions = append(conditions, "branch.name = ?")
			values = append(values, filter.Branch.Name)
		}
	}
	query := getCommit
	if len(conditions) > 0 {
		query += "\n" + fmt.Sprintf("WHERE %s", strings.Join(conditions, " AND "))
	}
	// Compute ORDER BY
	var orderByGeneric []OrderByColumn[commitColumn]
	if len(orderBys) == 0 {
		orderByGeneric = []OrderByColumn[commitColumn]{{Column: CommitColumnID, Order: SortOrderAsc}}
	} else {
		for _, orderBy := range orderBys {
			orderByGeneric = append(orderByGeneric, OrderByColumn[commitColumn](orderBy))
		}
	}
	query += "\n" + OrderByQuery[commitColumn](orderByGeneric...)
	query = extCtx.Rebind(query)
	return &CommitIterator{
		paginator: newPageIterator[CommitRow](ctx, query, values, startPage, pageSize, 0),
		extCtx:    extCtx,
	}, nil
}

func ForEachCommit(ctx context.Context, db *pachsql.DB, filter *pfs.Commit, cb func(commit Commit) error, orderBys ...OrderByCommitColumn) error {
	iter, err := NewCommitsIterator(ctx, db, 0, 100, filter, orderBys...)
	if err != nil {
		return errors.Wrap(err, "for each commit")
	}
	if err := stream.ForEach[Commit](ctx, iter, cb); err != nil {
		return errors.Wrap(err, "for each commit")
	}
	return nil
}

func ForEachCommitTxByFilter(ctx context.Context, tx *pachsql.Tx, filter *pfs.Commit, cb func(commit Commit) error, orderBys ...OrderByCommitColumn) error {
	if filter == nil {
		return errors.Errorf("filter cannot be empty")
	}
	iter, err := NewCommitsIterator(ctx, tx, 0, commitsPageSize, filter, orderBys...)
	if err != nil {
		return errors.Wrap(err, "for each commit tx by filter")
	}
	if err := stream.ForEach[Commit](ctx, iter, func(commit Commit) error {
		return cb(commit)
	}); err != nil {
		return errors.Wrap(err, "for each commit tx by filter")
	}
	return nil
}

func ListCommitTxByFilter(ctx context.Context, tx *pachsql.Tx, filter *pfs.Commit, orderBys ...OrderByCommitColumn) ([]*Commit, error) {
	var commits []*Commit
	if err := ForEachCommitTxByFilter(ctx, tx, filter, func(commit Commit) error {
		commitPtr := commit // The address of commit is static and the reference is overwritten each iteration, so a copy has to be allocated instead.
		commits = append(commits, &commitPtr)
		return nil
	}, orderBys...); err != nil {
		return nil, errors.Wrap(err, "list commits tx by filter")
	}
	return commits, nil
}

func ListCommitInfoTxByFilter(ctx context.Context, tx *pachsql.Tx, filter *pfs.Commit, orderBys ...OrderByCommitColumn) ([]*pfs.CommitInfo, error) {
	var commits []*pfs.CommitInfo
	if err := ForEachCommitTxByFilter(ctx, tx, filter, func(commit Commit) error {
		commits = append(commits, commit.CommitInfo)
		return nil
	}, orderBys...); err != nil {
		return nil, errors.Wrap(err, "list commits tx by filter")
	}
	return commits, nil
}

// Helper functions for watching commits.
type commitUpsertHandler func(commit Commit) error
type commitDeleteHandler func(id CommitID) error

// WatchCommits creates a watcher and watches the pfs.commits table for changes.
func WatchCommits(ctx context.Context, db *pachsql.DB, listener collection.PostgresListener, onUpsert commitUpsertHandler, onDelete commitDeleteHandler) error {
	watcher, err := postgres.NewWatcher(db, listener, randutil.UniqueString("watch-commits-"), CommitsChannelName)
	if err != nil {
		return err
	}
	defer watcher.Close()
	snapshot, err := NewCommitsIterator(ctx, db, 0, commitsPageSize, nil, OrderByCommitColumn{Column: CommitColumnID, Order: SortOrderAsc})
	if err != nil {
		return err
	}
	return watchCommits(ctx, db, snapshot, watcher.Watch(), onUpsert, onDelete)
}

// WatchCommitsInRepo creates a watcher and watches for commits in a repo.
func WatchCommitsInRepo(ctx context.Context, db *pachsql.DB, listener collection.PostgresListener, repoID RepoID, onUpsert commitUpsertHandler, onDelete commitDeleteHandler) error {
	watcher, err := postgres.NewWatcher(db, listener, randutil.UniqueString(fmt.Sprintf("watch-commits-in-repo-%d", repoID)), CommitsInRepoChannel(repoID))
	if err != nil {
		return err
	}
	defer watcher.Close()
	// Optimized query for getting commits in a repo.
	query := getCommit + fmt.Sprintf(" WHERE %s = ?  ORDER BY %s ASC", CommitColumnRepoID, CommitColumnID)
	query = db.Rebind(query)
	snapshot := &CommitIterator{paginator: newPageIterator[CommitRow](ctx, query, []any{repoID}, 0, commitsPageSize, 0), extCtx: db}
	return watchCommits(ctx, db, snapshot, watcher.Watch(), onUpsert, onDelete)
}

// WatchCommit creates a watcher and watches for changes to a single commit.
func WatchCommit(ctx context.Context, db *pachsql.DB, listener collection.PostgresListener, commitID CommitID, onUpsert commitUpsertHandler, onDelete commitDeleteHandler) error {
	watcher, err := postgres.NewWatcher(db, listener, randutil.UniqueString(fmt.Sprintf("watch-commit-%d-", commitID)), fmt.Sprintf("%s%d", CommitChannelName, commitID))
	if err != nil {
		return err
	}
	defer watcher.Close()
	var commit Commit
	if err := dbutil.WithTx(ctx, db, func(cbCtx context.Context, tx *pachsql.Tx) error {
		commitInfo, err := GetCommitInfo(ctx, tx, commitID)
		if err != nil {
			return errors.Wrap(err, "watch commit")
		}
		commit = Commit{ID: commitID, CommitInfo: commitInfo}
		return nil
	}); err != nil {
		return err
	}
	snapshot := stream.NewSlice([]Commit{commit})
	return watchCommits(ctx, db, snapshot, watcher.Watch(), onUpsert, onDelete)
}

func watchCommits(ctx context.Context, db *pachsql.DB, snapshot stream.Iterator[Commit], events <-chan *postgres.Event, onUpsert commitUpsertHandler, onDelete commitDeleteHandler) error {
	// Handle snapshot
	if err := stream.ForEach[Commit](ctx, snapshot, func(commit Commit) error {
		return onUpsert(commit)
	}); err != nil {
		return err
	}
	// Handle delta
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return errors.Errorf("watcher closed")
			}
			if event.Err != nil {
				return event.Err
			}
			id := CommitID(event.Id)
			switch event.Type {
			case postgres.EventDelete:
				if err := onDelete(id); err != nil {
					return err
				}
			case postgres.EventInsert, postgres.EventUpdate:
				var commitInfo *pfs.CommitInfo
				if err := dbutil.WithTx(ctx, db, func(ctx context.Context, tx *pachsql.Tx) error {
					var err error
					commitInfo, err = GetCommitInfo(ctx, tx, id)
					if err != nil {
						return err
					}
					return nil
				}); err != nil {
					return err
				}
				if err := onUpsert(Commit{ID: id, CommitInfo: commitInfo}); err != nil {
					return err
				}
			default:
				return errors.Errorf("unknown event type: %v", event.Type)
			}
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "watcher cancelled")
		}
	}
}

func PickCommit(ctx context.Context, commitPicker *pfs.CommitPicker, tx *pachsql.Tx) (*Commit, error) {
	if commitPicker == nil || commitPicker.Picker == nil {
		return nil, errors.New("commit picker cannot be nil")
	}
	switch commitPicker.Picker.(type) {
	case *pfs.CommitPicker_Id:
		return pickCommitGlobalID(ctx, commitPicker.GetId(), tx)
	case *pfs.CommitPicker_BranchHead:
		return pickCommitBranchHead(ctx, commitPicker.GetBranchHead(), tx)
	case *pfs.CommitPicker_Ancestor:
		return pickCommitAncestorOf(ctx, commitPicker.GetAncestor(), tx)
	case *pfs.CommitPicker_BranchRoot_:
		return pickCommitBranchRoot(ctx, commitPicker.GetBranchRoot(), tx)
	default:
		return nil, errors.Errorf("commit picker is of an unknown type: %T", commitPicker.Picker)
	}
}

func pickCommitGlobalID(ctx context.Context, picker *pfs.CommitPicker_CommitByGlobalId, tx *pachsql.Tx) (*Commit, error) {
	repo, err := PickRepo(ctx, picker.Repo, tx)
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	commit, err := GetCommitByKey(ctx, tx, &pfs.Commit{
		Repo: repo.RepoInfo.Repo,
		Id:   picker.Id,
	})
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	return commit, nil
}

func pickCommitBranchHead(ctx context.Context, branchHead *pfs.BranchPicker, tx *pachsql.Tx) (*Commit, error) {
	branch, err := PickBranch(ctx, branchHead, tx)
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	commit, err := GetCommitByKey(ctx, tx, branch.Head)
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	return commit, nil
}

func pickCommitAncestorOf(ctx context.Context, ancestorOf *pfs.CommitPicker_AncestorOf, tx *pachsql.Tx) (*Commit, error) {
	startCommit, err := PickCommit(ctx, ancestorOf.Start, tx)
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	if ancestorOf.Offset == 0 {
		return startCommit, nil
	}
	offset := 0
	commitPtr := startCommit.ID
	if err := forEachCommitAncestorUntilRoot(ctx, tx, startCommit.ID, func(parentId, _ CommitID) error {
		if uint32(offset) == ancestorOf.Offset {
			return errutil.ErrBreak
		}
		commitPtr = parentId
		offset++
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	if uint32(offset) != ancestorOf.Offset {
		return nil, errors.Errorf("picking commit: invalid offset for ancestor of commit: %s, offset requested: %d, offset traversable: %d",
			CommitKey(startCommit.Commit), ancestorOf.Offset, offset)
	}
	commitInfo, err := GetCommitInfo(ctx, tx, commitPtr)
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	commit := &Commit{
		ID:         commitPtr,
		CommitInfo: commitInfo,
	}
	return commit, nil
}

func pickCommitBranchRoot(ctx context.Context, branchRoot *pfs.CommitPicker_BranchRoot, tx *pachsql.Tx) (*Commit, error) {
	headCommit, err := pickCommitBranchHead(ctx, branchRoot.Branch, tx)
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	pathToRoot := []CommitID{headCommit.ID}
	depthToRoot := 1
	if err := forEachCommitAncestorUntilRoot(ctx, tx, headCommit.ID, func(parentId, _ CommitID) error {
		pathToRoot = append(pathToRoot, parentId)
		depthToRoot++
		if uint32(len(pathToRoot)) > branchRoot.Offset+1 { //+1 here handles case where offset is 0.
			pathToRoot = pathToRoot[1:] // this creates a 'sliding window'. If the window is full, drop the first item.
		}
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	if uint32(depthToRoot) < branchRoot.Offset {
		return nil, errors.Errorf("picking commit: invalid offset from branch root for head commit: %s, offset: %d, maximum depth: %d",
			CommitKey(headCommit.Commit), branchRoot.Offset, depthToRoot)
	}
	if len(pathToRoot) == 0 {
		return nil, errors.Errorf("picking commit: branch root not found for head commit: %s", CommitKey(headCommit.Commit))
	}
	commitInfo, err := GetCommitInfo(ctx, tx, pathToRoot[0])
	if err != nil {
		return nil, errors.Wrap(err, "picking commit")
	}
	commit := &Commit{
		ID:         pathToRoot[0],
		CommitInfo: commitInfo,
	}
	return commit, nil
}
