package iavl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bvinc/go-sqlite-lite/sqlite3"
	"github.com/dustin/go-humanize"
	"github.com/rs/zerolog"
)

func (sql *SqliteDb) Snapshot(ctx context.Context, tree *Tree, version int64) error {
	err := sql.leafWrite.Exec(
		fmt.Sprintf("CREATE TABLE snapshot_%d (ordinal int, version int, sequence int, bytes blob);", version))
	if err != nil {
		return err
	}
	err = tree.LoadVersion(version)
	if err != nil {
		return err
	}

	snapshot := &sqliteSnapshot{
		ctx:       ctx,
		sql:       sql,
		batchSize: 200_000,
		version:   version,
		log:       log.With().Str("path", filepath.Base(sql.opts.Path)).Logger(),
		getLeft: func(node *Node) *Node {
			return node.left(tree)
		},
		getRight: func(node *Node) *Node {
			return node.right(tree)
		},
	}
	if err = snapshot.prepareWrite(); err != nil {
		return err
	}
	if err = snapshot.writeStep(tree.root); err != nil {
		return err
	}
	if err = snapshot.flush(); err != nil {
		return err
	}
	log.Info().Str("path", sql.opts.Path).Msgf("creating index on snapshot_%d", version)
	err = sql.leafWrite.Exec(fmt.Sprintf("CREATE INDEX snapshot_%d_idx ON snapshot_%d (ordinal);", version, version))
	return err
}

type SnapshotOptions struct {
	StoreLeafValues bool
	SaveTree        bool
}

func (sql *SqliteDb) WriteSnapshot(
	ctx context.Context, version int64, nextFn func() *SnapshotNode, opts SnapshotOptions,
) (*Node, error) {
	snap := &sqliteSnapshot{
		ctx:       ctx,
		sql:       sql,
		batchSize: 200_000,
		version:   version,
		lastWrite: time.Now(),
		log:       log.With().Str("path", filepath.Base(sql.opts.Path)).Logger(),
	}
	if opts.SaveTree {
		if err := sql.NextShard(); err != nil {
			return nil, err
		}
	}
	err := snap.sql.leafWrite.Exec(
		fmt.Sprintf("CREATE TABLE snapshot_%d (ordinal int, version int, sequence int, bytes blob);", version))
	if err != nil {
		return nil, err
	}
	if err = snap.prepareWrite(); err != nil {
		return nil, err
	}

	var (
		step           func() (*Node, error)
		maybeFlush     func() error
		count          int
		uniqueVersions = make(map[int64]struct{})
	)
	maybeFlush = func() error {
		count++
		if count%snap.batchSize == 0 {
			if err = snap.flush(); err != nil {
				return err
			}
			if err = snap.prepareWrite(); err != nil {
				return err
			}
		}
		return nil
	}

	step = func() (*Node, error) {
		snapshotNode := nextFn()
		ordinal := snap.ordinal
		snap.ordinal++

		node := &Node{
			key:           snapshotNode.Key,
			subtreeHeight: snapshotNode.Height,
			nodeKey:       NewNodeKey(snapshotNode.Version, uint32(ordinal)),
		}
		if node.subtreeHeight == 0 {
			node.value = snapshotNode.Value
			node.size = 1
			node._hash(snapshotNode.Version)
			if !opts.StoreLeafValues {
				node.value = nil
			}
			nodeBz, err := node.Bytes()
			if err != nil {
				return nil, err
			}
			if err = snap.snapshotInsert.Exec(ordinal, snapshotNode.Version, ordinal, nodeBz); err != nil {
				return nil, err
			}
			if err = snap.leafInsert.Exec(snapshotNode.Version, ordinal, nodeBz); err != nil {
				return nil, err
			}
			if err = maybeFlush(); err != nil {
				return nil, err
			}
			return node, nil
		}

		node.leftNode, err = step()
		if err != nil {
			return nil, err
		}
		node.leftNodeKey = node.leftNode.nodeKey
		node.rightNode, err = step()
		if err != nil {
			return nil, err
		}
		node.rightNodeKey = node.rightNode.nodeKey

		node.size = node.leftNode.size + node.rightNode.size
		node._hash(snapshotNode.Version)
		node.leftNode = nil
		node.rightNode = nil

		nodeBz, err := node.Bytes()
		if err != nil {
			return nil, err
		}
		if err = snap.snapshotInsert.Exec(ordinal, snapshotNode.Version, ordinal, nodeBz); err != nil {
			return nil, err
		}
		if err = snap.treeInsert.Exec(snapshotNode.Version, ordinal, nodeBz); err != nil {
			return nil, err
		}
		uniqueVersions[snapshotNode.Version] = struct{}{}
		if err = maybeFlush(); err != nil {
			return nil, err
		}
		return node, nil
	}
	root, err := step()
	if err != nil {
		return nil, err
	}

	if err = snap.flush(); err != nil {
		return nil, err
	}

	var versions []int64
	for v := range uniqueVersions {
		versions = append(versions, v)
	}
	if err = sql.MapVersions(versions, sql.shardId); err != nil {
		return nil, err
	}

	log.Info().Str("path", sql.opts.Path).Msg("creating table indexes d")
	err = sql.leafWrite.Exec(fmt.Sprintf("CREATE INDEX snapshot_%d_idx ON snapshot_%d (ordinal);", version, version))
	if err != nil {
		return nil, err
	}
	err = snap.sql.treeWrite.Exec(fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS tree_idx_%d ON tree_%d (version, sequence);",
		snap.sql.shardId, snap.sql.shardId))
	if err != nil {
		return nil, err
	}

	return root, nil
}

type SnapshotNode struct {
	Key     []byte
	Value   []byte
	Version int64
	Height  int8
}

func (sql *SqliteDb) ImportSnapshotFromTable(version int64, loadLeaves bool) (*Node, error) {
	read, err := sql.getReadConn()
	if err != nil {
		return nil, err
	}
	q, err := read.Prepare(fmt.Sprintf("SELECT version, sequence, bytes FROM snapshot_%d ORDER BY ordinal", version))
	if err != nil {
		return nil, err
	}
	defer func(q *sqlite3.Stmt) {
		err = q.Close()
		if err != nil {
			log.Error().Err(err).Msg("error closing import query")
		}
	}(q)

	imp := &sqliteImport{
		query:      q,
		pool:       sql.pool,
		loadLeaves: loadLeaves,
		since:      time.Now(),
		log:        log.With().Str("path", sql.opts.Path).Logger(),
	}
	root, err := imp.queryStep()
	if err != nil {
		return nil, err
	}

	if !loadLeaves {
		return root, nil
	}

	h := root.hash
	rehashTree(root)
	if !bytes.Equal(h, root.hash) {
		return nil, fmt.Errorf("rehash failed; expected=%x, got=%x", h, root.hash)
	}

	return root, nil
}

func (sql *SqliteDb) ImportMostRecentSnapshot(targetVersion int64, loadLeaves bool) (*Node, int64, error) {
	read, err := sql.getReadConn()
	if err != nil {
		return nil, 0, err
	}
	q, err := read.Prepare("SELECT tbl_name FROM changelog.sqlite_master WHERE type='table' AND name LIKE 'snapshot_%' ORDER BY name DESC")
	defer func(q *sqlite3.Stmt) {
		err = q.Close()
		if err != nil {
			log.Error().Err(err).Msg("error closing import query")
		}
	}(q)
	if err != nil {
		return nil, 0, err
	}

	var (
		name    string
		version int64
	)
	for {
		ok, err := q.Step()
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			return nil, 0, fmt.Errorf("no prior snapshot found version=%d path=%s", targetVersion, sql.opts.Path)
		}
		err = q.Scan(&name)
		if err != nil {
			return nil, 0, err
		}
		vs := name[len("snapshot_"):]
		if vs == "" {
			return nil, 0, fmt.Errorf("unexpected snapshot table name %s", name)
		}
		version, err = strconv.ParseInt(vs, 10, 64)
		if err != nil {
			return nil, 0, err
		}
		if version <= targetVersion {
			break
		}
	}

	root, err := sql.ImportSnapshotFromTable(version, loadLeaves)
	if err != nil {
		return nil, 0, err
	}
	return root, version, err
}

func FindDbsInPath(path string) ([]string, error) {
	var paths []string
	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Base(path) == "changelog.sqlite" {
			paths = append(paths, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

type sqliteSnapshot struct {
	ctx context.Context
	sql *SqliteDb

	snapshotInsert *sqlite3.Stmt
	leafInsert     *sqlite3.Stmt
	treeInsert     *sqlite3.Stmt

	lastWrite time.Time
	ordinal   int
	batchSize int
	version   int64
	getLeft   func(*Node) *Node
	getRight  func(*Node) *Node
	log       zerolog.Logger
}

// TODO
// merge these two functions

func (snap *sqliteSnapshot) writeStep(node *Node) error {
	snap.ordinal++
	// Pre-order, NLR traversal
	// Visit this node
	nodeBz, err := node.Bytes()
	if err != nil {
		return err
	}
	err = snap.snapshotInsert.Exec(snap.ordinal, node.nodeKey.Version(), int(node.nodeKey.Sequence()), nodeBz)
	if err != nil {
		return err
	}

	if snap.ordinal%snap.batchSize == 0 {
		if err = snap.flush(); err != nil {
			return err
		}
		if err = snap.prepareWrite(); err != nil {
			return err
		}
	}

	if node.isLeaf() {
		return nil
	}

	// traverse left
	err = snap.writeStep(snap.getLeft(node))
	if err != nil {
		return err
	}

	// traverse right
	return snap.writeStep(snap.getRight(node))
}

func (snap *sqliteSnapshot) flush() error {
	select {
	case <-snap.ctx.Done():
		snap.log.Info().Msgf("snapshot cancelled at ordinal=%s", humanize.Comma(int64(snap.ordinal)))
		return errors.Join(
			snap.snapshotInsert.Reset(),
			snap.snapshotInsert.Close(),
			snap.leafInsert.Reset(),
			snap.leafInsert.Close(),
			snap.treeInsert.Reset(),
			snap.treeInsert.Close(),
			snap.sql.leafWrite.Rollback(),
			snap.sql.leafWrite.Close(),
			snap.sql.treeWrite.Rollback(),
			snap.sql.treeWrite.Close())
	default:
	}

	snap.log.Info().Msgf("flush total=%s size=%s dur=%s wr/s=%s",
		humanize.Comma(int64(snap.ordinal)),
		humanize.Comma(int64(snap.batchSize)),
		time.Since(snap.lastWrite).Round(time.Millisecond),
		humanize.Comma(int64(float64(snap.batchSize)/time.Since(snap.lastWrite).Seconds())),
	)

	err := errors.Join(
		snap.sql.leafWrite.Commit(),
		snap.sql.treeWrite.Commit(),
		snap.snapshotInsert.Close(),
		snap.leafInsert.Close(),
		snap.treeInsert.Close(),
	)
	snap.lastWrite = time.Now()
	return err
}

func (snap *sqliteSnapshot) prepareWrite() error {
	err := snap.sql.leafWrite.Begin()
	if err != nil {
		return err
	}
	err = snap.sql.treeWrite.Begin()
	if err != nil {
		return err
	}

	snap.snapshotInsert, err = snap.sql.leafWrite.Prepare(
		fmt.Sprintf("INSERT INTO snapshot_%d (ordinal, version, sequence, bytes) VALUES (?, ?, ?, ?);",
			snap.version))
	snap.leafInsert, err = snap.sql.leafWrite.Prepare("INSERT INTO leaf (version, sequence, bytes) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	snap.treeInsert, err = snap.sql.treeWrite.Prepare(
		fmt.Sprintf("INSERT INTO tree_%d (version, sequence, bytes) VALUES (?, ?, ?)", snap.sql.shardId))
	return err
}

func rehashTree(node *Node) {
	if node.isLeaf() {
		return
	}
	node.hash = nil

	rehashTree(node.leftNode)
	rehashTree(node.rightNode)

	node._hash(node.nodeKey.Version())
}

type sqliteImport struct {
	query      *sqlite3.Stmt
	pool       *NodePool
	loadLeaves bool

	i     int64
	since time.Time
	log   zerolog.Logger
}

func (sqlImport *sqliteImport) queryStep() (node *Node, err error) {
	sqlImport.i++
	if sqlImport.i%1_000_000 == 0 {
		sqlImport.log.Debug().Msgf("import: nodes=%s, node/s=%s",
			humanize.Comma(sqlImport.i),
			humanize.Comma(int64(float64(1_000_000)/time.Since(sqlImport.since).Seconds())),
		)
		sqlImport.since = time.Now()
	}

	hasRow, err := sqlImport.query.Step()
	if !hasRow {
		return nil, sqlImport.query.Reset()
	}
	if err != nil {
		return nil, err
	}
	var bz sqlite3.RawBytes
	var version, seq int
	err = sqlImport.query.Scan(&version, &seq, &bz)
	if err != nil {
		return nil, err
	}
	nodeKey := NewNodeKey(int64(version), uint32(seq))
	node, err = MakeNode(sqlImport.pool, nodeKey, bz)
	if err != nil {
		return nil, err
	}

	if node.isLeaf() {
		if sqlImport.loadLeaves {
			return node, nil
		}
		sqlImport.pool.Put(node)
		return nil, nil
	}

	node.leftNode, err = sqlImport.queryStep()
	if err != nil {
		return nil, err
	}
	node.rightNode, err = sqlImport.queryStep()
	if err != nil {
		return nil, err
	}
	return node, nil
}
