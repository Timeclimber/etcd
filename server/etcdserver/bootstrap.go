// Copyright 2021 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/pkg/v3/pbutil"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.etcd.io/etcd/server/v3/config"
	"go.etcd.io/etcd/server/v3/etcdserver/api"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/server/v3/etcdserver/api/snap"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v2discovery"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v2store"
	"go.etcd.io/etcd/server/v3/etcdserver/cindex"
	serverstorage "go.etcd.io/etcd/server/v3/storage"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/schema"
	"go.etcd.io/etcd/server/v3/storage/wal"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
)

func bootstrap(cfg config.ServerConfig) (b *bootstrappedServer, err error) {

	if cfg.MaxRequestBytes > recommendedMaxRequestBytes {
		cfg.Logger.Warn(
			"exceeded recommended request limit",
			zap.Uint("max-request-bytes", cfg.MaxRequestBytes),
			zap.String("max-request-size", humanize.Bytes(uint64(cfg.MaxRequestBytes))),
			zap.Int("recommended-request-bytes", recommendedMaxRequestBytes),
			zap.String("recommended-request-size", recommendedMaxRequestBytesString),
		)
	}

	if terr := fileutil.TouchDirAll(cfg.DataDir); terr != nil {
		return nil, fmt.Errorf("cannot access data directory: %v", terr)
	}
	ss := bootstrapSnapshot(cfg)
	prt, err := rafthttp.NewRoundTripper(cfg.PeerTLSInfo, cfg.PeerDialTimeout())
	if err != nil {
		return nil, err
	}

	if terr := fileutil.TouchDirAll(cfg.MemberDir()); terr != nil {
		return nil, fmt.Errorf("cannot access member directory: %v", terr)
	}

	storage, err := bootstrapStorage(cfg, ss, prt)
	if err != nil {
		return nil, err
	}

	cluster, err := bootstrapCluster(cfg, storage, prt, ss)
	if err != nil {
		storage.be.Close()
		return nil, err
	}
	return &bootstrappedServer{
		prt:     prt,
		ss:      ss,
		storage: storage,
		cluster: cluster,
	}, nil
}

type bootstrappedServer struct {
	storage *bootstrappedStorage
	cluster *bootstrapedCluster
	prt     http.RoundTripper
	ss      *snap.Snapshotter
}

type bootstrappedStorage struct {
	beHooks *serverstorage.BackendHooks
	st      v2store.Store
	be      backend.Backend
	ci      cindex.ConsistentIndexer
	beExist bool
}

type bootstrapedCluster struct {
	raft              *bootstrappedRaft
	remotes           []*membership.Member
	wal               *bootstrappedWAL
	clusterID, nodeID types.ID
}

type bootstrappedRaft struct {
	lg        *zap.Logger
	heartbeat time.Duration

	peers   []raft.Peer
	config  *raft.Config
	cl      *membership.RaftCluster
	storage *raft.MemoryStorage
}

func bootstrapStorage(cfg config.ServerConfig, ss *snap.Snapshotter, prt http.RoundTripper) (b *bootstrappedStorage, err error) {
	st := v2store.New(StoreClusterPrefix, StoreKeysPrefix)

	be, ci, beExist, beHooks, err := bootstrapBackend(cfg)
	if err != nil {
		return nil, err
	}
	return &bootstrappedStorage{
		beHooks: beHooks,
		st:      st,
		be:      be,
		ci:      ci,
		beExist: beExist,
	}, nil
}

func bootstrapSnapshot(cfg config.ServerConfig) *snap.Snapshotter {
	if err := fileutil.TouchDirAll(cfg.SnapDir()); err != nil {
		cfg.Logger.Fatal(
			"failed to create snapshot directory",
			zap.String("path", cfg.SnapDir()),
			zap.Error(err),
		)
	}

	if err := fileutil.RemoveMatchFile(cfg.Logger, cfg.SnapDir(), func(fileName string) bool {
		return strings.HasPrefix(fileName, "tmp")
	}); err != nil {
		cfg.Logger.Error(
			"failed to remove temp file(s) in snapshot directory",
			zap.String("path", cfg.SnapDir()),
			zap.Error(err),
		)
	}
	return snap.New(cfg.Logger, cfg.SnapDir())
}

func bootstrapBackend(cfg config.ServerConfig) (be backend.Backend, ci cindex.ConsistentIndexer, beExist bool, beHooks *serverstorage.BackendHooks, err error) {
	beExist = fileutil.Exist(cfg.BackendPath())
	ci = cindex.NewConsistentIndex(nil)
	beHooks = serverstorage.NewBackendHooks(cfg.Logger, ci)
	be = serverstorage.OpenBackend(cfg, beHooks)
	defer func() {
		if err != nil && be != nil {
			be.Close()
		}
	}()
	ci.SetBackend(be)
	schema.CreateMetaBucket(be.BatchTx())
	if cfg.ExperimentalBootstrapDefragThresholdMegabytes != 0 {
		err = maybeDefragBackend(cfg, be)
		if err != nil {
			return nil, nil, false, nil, err
		}
	}
	cfg.Logger.Debug("restore consistentIndex", zap.Uint64("index", ci.ConsistentIndex()))

	// TODO(serathius): Implement schema setup in fresh storage
	if beExist {
		err = schema.Validate(cfg.Logger, be.BatchTx())
		if err != nil {
			cfg.Logger.Error("Failed to validate schema", zap.Error(err))
			return nil, nil, false, nil, err
		}
	}
	return be, ci, beExist, beHooks, nil
}

func maybeDefragBackend(cfg config.ServerConfig, be backend.Backend) error {
	size := be.Size()
	sizeInUse := be.SizeInUse()
	freeableMemory := uint(size - sizeInUse)
	thresholdBytes := cfg.ExperimentalBootstrapDefragThresholdMegabytes * 1024 * 1024
	if freeableMemory < thresholdBytes {
		cfg.Logger.Info("Skipping defragmentation",
			zap.Int64("current-db-size-bytes", size),
			zap.String("current-db-size", humanize.Bytes(uint64(size))),
			zap.Int64("current-db-size-in-use-bytes", sizeInUse),
			zap.String("current-db-size-in-use", humanize.Bytes(uint64(sizeInUse))),
			zap.Uint("experimental-bootstrap-defrag-threshold-bytes", thresholdBytes),
			zap.String("experimental-bootstrap-defrag-threshold", humanize.Bytes(uint64(thresholdBytes))),
		)
		return nil
	}
	return be.Defrag()
}

func bootstrapCluster(cfg config.ServerConfig, storage *bootstrappedStorage, prt http.RoundTripper, ss *snap.Snapshotter) (c *bootstrapedCluster, err error) {
	haveWAL := wal.Exist(cfg.WALDir())
	switch {
	case !haveWAL && !cfg.NewCluster:
		c, err = bootstrapExistingClusterNoWAL(cfg, prt, storage.st, storage.be)
	case !haveWAL && cfg.NewCluster:
		c, err = bootstrapNewClusterNoWAL(cfg, prt, storage.st, storage.be)
	case haveWAL:
		c, err = bootstrapClusterWithWAL(cfg, storage, ss)
	default:
		return nil, fmt.Errorf("unsupported bootstrap config")
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func bootstrapExistingClusterNoWAL(cfg config.ServerConfig, prt http.RoundTripper, st v2store.Store, be backend.Backend) (*bootstrapedCluster, error) {
	if err := cfg.VerifyJoinExisting(); err != nil {
		return nil, err
	}
	cl, err := membership.NewClusterFromURLsMap(cfg.Logger, cfg.InitialClusterToken, cfg.InitialPeerURLsMap)
	if err != nil {
		return nil, err
	}
	existingCluster, gerr := GetClusterFromRemotePeers(cfg.Logger, getRemotePeerURLs(cl, cfg.Name), prt)
	if gerr != nil {
		return nil, fmt.Errorf("cannot fetch cluster info from peer urls: %v", gerr)
	}
	if err := membership.ValidateClusterAndAssignIDs(cfg.Logger, cl, existingCluster); err != nil {
		return nil, fmt.Errorf("error validating peerURLs %s: %v", existingCluster, err)
	}
	if !isCompatibleWithCluster(cfg.Logger, cl, cl.MemberByName(cfg.Name).ID, prt) {
		return nil, fmt.Errorf("incompatible with current running cluster")
	}

	remotes := existingCluster.Members()
	cl.SetID(types.ID(0), existingCluster.ID())
	cl.SetStore(st)
	cl.SetBackend(schema.NewMembershipBackend(cfg.Logger, be))
	member := cl.MemberByName(cfg.Name)
	bwal := bootstrapNewWAL(cfg, member, cl)
	br := bootstrapRaftFromCluster(cfg, cl, nil, bwal)
	cl.SetID(member.ID, existingCluster.ID())
	return &bootstrapedCluster{
		raft:      br,
		remotes:   remotes,
		wal:       bwal,
		clusterID: cl.ID(),
		nodeID:    member.ID,
	}, nil
}

func bootstrapNewClusterNoWAL(cfg config.ServerConfig, prt http.RoundTripper, st v2store.Store, be backend.Backend) (*bootstrapedCluster, error) {
	if err := cfg.VerifyBootstrap(); err != nil {
		return nil, err
	}
	cl, err := membership.NewClusterFromURLsMap(cfg.Logger, cfg.InitialClusterToken, cfg.InitialPeerURLsMap)
	if err != nil {
		return nil, err
	}
	m := cl.MemberByName(cfg.Name)
	if isMemberBootstrapped(cfg.Logger, cl, cfg.Name, prt, cfg.BootstrapTimeoutEffective()) {
		return nil, fmt.Errorf("member %s has already been bootstrapped", m.ID)
	}
	if cfg.ShouldDiscover() {
		var str string
		str, err = v2discovery.JoinCluster(cfg.Logger, cfg.DiscoveryURL, cfg.DiscoveryProxy, m.ID, cfg.InitialPeerURLsMap.String())
		if err != nil {
			return nil, &DiscoveryError{Op: "join", Err: err}
		}
		var urlsmap types.URLsMap
		urlsmap, err = types.NewURLsMap(str)
		if err != nil {
			return nil, err
		}
		if config.CheckDuplicateURL(urlsmap) {
			return nil, fmt.Errorf("discovery cluster %s has duplicate url", urlsmap)
		}
		if cl, err = membership.NewClusterFromURLsMap(cfg.Logger, cfg.InitialClusterToken, urlsmap); err != nil {
			return nil, err
		}
	}
	cl.SetStore(st)
	cl.SetBackend(schema.NewMembershipBackend(cfg.Logger, be))
	member := cl.MemberByName(cfg.Name)
	bwal := bootstrapNewWAL(cfg, member, cl)
	br := bootstrapRaftFromCluster(cfg, cl, cl.MemberIDs(), bwal)
	cl.SetID(member.ID, cl.ID())
	return &bootstrapedCluster{
		remotes:   nil,
		raft:      br,
		wal:       bwal,
		clusterID: cl.ID(),
		nodeID:    member.ID,
	}, nil
}

func bootstrapClusterWithWAL(cfg config.ServerConfig, storage *bootstrappedStorage, ss *snap.Snapshotter) (*bootstrapedCluster, error) {
	if err := fileutil.IsDirWriteable(cfg.MemberDir()); err != nil {
		return nil, fmt.Errorf("cannot write to member directory: %v", err)
	}

	if err := fileutil.IsDirWriteable(cfg.WALDir()); err != nil {
		return nil, fmt.Errorf("cannot write to WAL directory: %v", err)
	}

	if cfg.ShouldDiscover() {
		cfg.Logger.Warn(
			"discovery token is ignored since cluster already initialized; valid logs are found",
			zap.String("bwal-dir", cfg.WALDir()),
		)
	}
	snapshot, err := recoverSnapshot(cfg, storage, ss)
	if err != nil {
		return nil, err
	}
	bwal, meta := bootstrapWALFromSnapshot(cfg.Logger, cfg.WALDir(), snapshot, cfg.UnsafeNoFsync)

	b := &bootstrapedCluster{
		wal:       bwal,
		clusterID: meta.clusterID,
		nodeID:    meta.nodeID,
	}
	if cfg.ForceNewCluster {
		// discard the previously uncommitted entries
		bwal.ents = bwal.CommitedEntries()
		entries := bwal.ConfigChangeEntries(meta)
		// force commit config change entries
		bwal.AppendAndCommitEntries(entries)
		cfg.Logger.Info(
			"forcing restart member",
			zap.String("cluster-id", meta.clusterID.String()),
			zap.String("local-member-id", meta.nodeID.String()),
			zap.Uint64("commit-index", bwal.st.Commit),
		)
	} else {
		cfg.Logger.Info(
			"restarting local member",
			zap.String("cluster-id", meta.clusterID.String()),
			zap.String("local-member-id", meta.nodeID.String()),
			zap.Uint64("commit-index", bwal.st.Commit),
		)
	}
	b.raft = bootstrapRaftFromSnapshot(cfg, bwal, meta)

	b.raft.cl.SetStore(storage.st)
	b.raft.cl.SetBackend(schema.NewMembershipBackend(cfg.Logger, storage.be))
	b.raft.cl.Recover(api.UpdateCapability)
	if b.raft.cl.Version() != nil && !b.raft.cl.Version().LessThan(semver.Version{Major: 3}) && !storage.beExist {
		bepath := cfg.BackendPath()
		os.RemoveAll(bepath)
		return nil, fmt.Errorf("database file (%v) of the backend is missing", bepath)
	}
	return b, nil
}

func recoverSnapshot(cfg config.ServerConfig, storage *bootstrappedStorage, ss *snap.Snapshotter) (*raftpb.Snapshot, error) {
	// Find a snapshot to start/restart a raft node
	walSnaps, err := wal.ValidSnapshotEntries(cfg.Logger, cfg.WALDir())
	if err != nil {
		return nil, err
	}
	// snapshot files can be orphaned if etcd crashes after writing them but before writing the corresponding
	// bwal log entries
	snapshot, err = ss.LoadNewestAvailable(walSnaps)
	if err != nil && err != snap.ErrNoSnapshot {
		return nil, err
	}

	if snapshot != nil {
		if err = storage.st.Recovery(snapshot.Data); err != nil {
			cfg.Logger.Panic("failed to recover from snapshot", zap.Error(err))
		}

		if err = serverstorage.AssertNoV2StoreContent(cfg.Logger, storage.st, cfg.V2Deprecation); err != nil {
			cfg.Logger.Error("illegal v2store content", zap.Error(err))
			return nil, err
		}

		cfg.Logger.Info(
			"recovered v2 store from snapshot",
			zap.Uint64("snapshot-index", snapshot.Metadata.Index),
			zap.String("snapshot-size", humanize.Bytes(uint64(snapshot.Size()))),
		)

		if storage.be, err = serverstorage.RecoverSnapshotBackend(cfg, storage.be, *snapshot, storage.beExist, storage.beHooks); err != nil {
			cfg.Logger.Panic("failed to recover v3 backend from snapshot", zap.Error(err))
		}
		s1, s2 := be.Size(), be.SizeInUse()
		cfg.Logger.Info(
			"recovered v3 backend from snapshot",
			zap.Int64("backend-size-bytes", s1),
			zap.String("backend-size", humanize.Bytes(uint64(s1))),
			zap.Int64("backend-size-in-use-bytes", s2),
			zap.String("backend-size-in-use", humanize.Bytes(uint64(s2))),
		)
		if storage.beExist {
			// TODO: remove kvindex != 0 checking when we do not expect users to upgrade
			// etcd from pre-3.0 release.
			kvindex := storage.ci.ConsistentIndex()
			if kvindex < snapshot.Metadata.Index {
				if kvindex != 0 {
					return nil, fmt.Errorf("database file (%v index %d) does not match with snapshot (index %d)", cfg.BackendPath(), kvindex, snapshot.Metadata.Index)
				}
				cfg.Logger.Warn(
					"consistent index was never saved",
					zap.Uint64("snapshot-index", snapshot.Metadata.Index),
				)
			}
		}
	} else {
		cfg.Logger.Info("No snapshot found. Recovering WAL from scratch!")
	}
	return snapshot, nil
}

func bootstrapRaftFromCluster(cfg config.ServerConfig, cl *membership.RaftCluster, ids []types.ID, bwal *bootstrappedWAL) *bootstrappedRaft {
	member := cl.MemberByName(cfg.Name)
	peers := make([]raft.Peer, len(ids))
	for i, id := range ids {
		var ctx []byte
		ctx, err := json.Marshal((*cl).Member(id))
		if err != nil {
			cfg.Logger.Panic("failed to marshal member", zap.Error(err))
		}
		peers[i] = raft.Peer{ID: uint64(id), Context: ctx}
	}
	cfg.Logger.Info(
		"starting local member",
		zap.String("local-member-id", member.ID.String()),
		zap.String("cluster-id", cl.ID().String()),
	)
	s := bwal.MemoryStorage()
	return &bootstrappedRaft{
		lg:        cfg.Logger,
		heartbeat: time.Duration(cfg.TickMs) * time.Millisecond,
		cl:        cl,
		config:    raftConfig(cfg, uint64(member.ID), s),
		peers:     peers,
		storage:   s,
	}
}

func bootstrapRaftFromSnapshot(cfg config.ServerConfig, bwal *bootstrappedWAL, meta *snapshotMetadata) *bootstrappedRaft {
	cl := membership.NewCluster(cfg.Logger)
	cl.SetID(meta.nodeID, meta.clusterID)
	s := bwal.MemoryStorage()
	return &bootstrappedRaft{
		lg:        cfg.Logger,
		heartbeat: time.Duration(cfg.TickMs) * time.Millisecond,
		cl:        cl,
		config:    raftConfig(cfg, uint64(meta.nodeID), s),
		storage:   s,
	}
}

func raftConfig(cfg config.ServerConfig, id uint64, s *raft.MemoryStorage) *raft.Config {
	return &raft.Config{
		ID:              id,
		ElectionTick:    cfg.ElectionTicks,
		HeartbeatTick:   1,
		Storage:         s,
		MaxSizePerMsg:   maxSizePerMsg,
		MaxInflightMsgs: maxInflightMsgs,
		CheckQuorum:     true,
		PreVote:         cfg.PreVote,
		Logger:          NewRaftLoggerZap(cfg.Logger.Named("raft")),
	}
}

func (b *bootstrappedRaft) newRaftNode(ss *snap.Snapshotter, wal *wal.WAL) *raftNode {
	var n raft.Node
	if len(b.peers) == 0 {
		n = raft.RestartNode(b.config)
	} else {
		n = raft.StartNode(b.config, b.peers)
	}
	raftStatusMu.Lock()
	raftStatus = n.Status
	raftStatusMu.Unlock()
	return newRaftNode(
		raftNodeConfig{
			lg:          b.lg,
			isIDRemoved: func(id uint64) bool { return b.cl.IsIDRemoved(types.ID(id)) },
			Node:        n,
			heartbeat:   b.heartbeat,
			raftStorage: b.storage,
			storage:     NewStorage(wal, ss),
		},
	)
}

// bootstrapWALFromSnapshot reads the WAL at the given snap and returns the wal, its latest HardState and cluster ID, and all entries that appear
// after the position of the given snap in the WAL.
// The snap must have been previously saved to the WAL, or this call will panic.
func bootstrapWALFromSnapshot(lg *zap.Logger, waldir string, snapshot *raftpb.Snapshot, unsafeNoFsync bool) (*bootstrappedWAL, *snapshotMetadata) {
	var walsnap walpb.Snapshot
	if snapshot != nil {
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	repaired := false
	for {
		w, err := wal.Open(lg, waldir, walsnap)
		if err != nil {
			lg.Fatal("failed to open WAL", zap.Error(err))
		}
		if unsafeNoFsync {
			w.SetUnsafeNoFsync()
		}
		wmetadata, st, ents, err := w.ReadAll()
		if err != nil {
			w.Close()
			// we can only repair ErrUnexpectedEOF and we never repair twice.
			if repaired || err != io.ErrUnexpectedEOF {
				lg.Fatal("failed to read WAL, cannot be repaired", zap.Error(err))
			}
			if !wal.Repair(lg, waldir) {
				lg.Fatal("failed to repair WAL", zap.Error(err))
			} else {
				lg.Info("repaired WAL", zap.Error(err))
				repaired = true
			}
			continue
		}
		var metadata etcdserverpb.Metadata
		pbutil.MustUnmarshal(&metadata, wmetadata)
		id := types.ID(metadata.NodeID)
		cid := types.ID(metadata.ClusterID)
		meta := &snapshotMetadata{clusterID: cid, nodeID: id}
		return &bootstrappedWAL{
			lg:       lg,
			w:        w,
			st:       &st,
			ents:     ents,
			snapshot: snapshot,
		}, meta
	}
}

type snapshotMetadata struct {
	nodeID, clusterID types.ID
}

func bootstrapNewWAL(cfg config.ServerConfig, m *membership.Member, cl *membership.RaftCluster) *bootstrappedWAL {
	metadata := pbutil.MustMarshal(
		&etcdserverpb.Metadata{
			NodeID:    uint64(m.ID),
			ClusterID: uint64(cl.ID()),
		},
	)
	w, err := wal.Create(cfg.Logger, cfg.WALDir(), metadata)
	if err != nil {
		cfg.Logger.Panic("failed to create WAL", zap.Error(err))
	}
	if cfg.UnsafeNoFsync {
		w.SetUnsafeNoFsync()
	}
	return &bootstrappedWAL{
		lg: cfg.Logger,
		w:  w,
	}
}

type bootstrappedWAL struct {
	lg *zap.Logger

	w        *wal.WAL
	st       *raftpb.HardState
	ents     []raftpb.Entry
	snapshot *raftpb.Snapshot
}

func (wal *bootstrappedWAL) MemoryStorage() *raft.MemoryStorage {
	s := raft.NewMemoryStorage()
	if wal.snapshot != nil {
		s.ApplySnapshot(*wal.snapshot)
	}
	if wal.st != nil {
		s.SetHardState(*wal.st)
	}
	if len(wal.ents) != 0 {
		s.Append(wal.ents)
	}
	return s
}

func (wal *bootstrappedWAL) CommitedEntries() []raftpb.Entry {
	for i, ent := range wal.ents {
		if ent.Index > wal.st.Commit {
			wal.lg.Info(
				"discarding uncommitted WAL entries",
				zap.Uint64("entry-index", ent.Index),
				zap.Uint64("commit-index-from-wal", wal.st.Commit),
				zap.Int("number-of-discarded-entries", len(wal.ents)-i),
			)
			return wal.ents[:i]
		}
	}
	return wal.ents
}

func (wal *bootstrappedWAL) ConfigChangeEntries(meta *snapshotMetadata) []raftpb.Entry {
	return serverstorage.CreateConfigChangeEnts(
		wal.lg,
		serverstorage.GetIDs(wal.lg, wal.snapshot, wal.ents),
		uint64(meta.nodeID),
		wal.st.Term,
		wal.st.Commit,
	)
}

func (wal *bootstrappedWAL) AppendAndCommitEntries(ents []raftpb.Entry) {
	wal.ents = append(wal.ents, ents...)
	err := wal.w.Save(raftpb.HardState{}, ents)
	if err != nil {
		wal.lg.Fatal("failed to save hard state and entries", zap.Error(err))
	}
	if len(wal.ents) != 0 {
		wal.st.Commit = wal.ents[len(wal.ents)-1].Index
	}
}
