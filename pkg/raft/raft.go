// This code has been modified from its original form by Cockroach Labs, Inc.
// All modifications are Copyright 2024 Cockroach Labs, Inc.
//
// Copyright 2015 The etcd Authors
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

package raft

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"

	"github.com/cockroachdb/cockroach/pkg/raft/confchange"
	"github.com/cockroachdb/cockroach/pkg/raft/quorum"
	pb "github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/raft/raftstoreliveness"
	"github.com/cockroachdb/cockroach/pkg/raft/tracker"
)

const (
	// None is a placeholder node ID used when there is no leader.
	//
	// TODO(arul): consider pulling these into raftpb as well.
	None pb.PeerID = 0
	// LocalAppendThread is a reference to a local thread that saves unstable
	// log entries and snapshots to stable storage. The identifier is used as a
	// target for MsgStorageAppend messages when AsyncStorageWrites is enabled.
	LocalAppendThread pb.PeerID = math.MaxUint64
	// LocalApplyThread is a reference to a local thread that applies committed
	// log entries to the local state machine. The identifier is used as a
	// target for MsgStorageApply messages when AsyncStorageWrites is enabled.
	LocalApplyThread pb.PeerID = math.MaxUint64 - 1
)

// Possible values for StateType.
const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
	StatePreCandidate
	numStates
)

// Possible values for CampaignType
const (
	// campaignPreElection represents the first phase of a normal election when
	// Config.PreVote is true.
	campaignPreElection CampaignType = "CampaignPreElection"
	// campaignElection represents a normal (time-based) election (the second phase
	// of the election when Config.PreVote is true).
	campaignElection CampaignType = "CampaignElection"
	// campaignTransfer represents the type of leader transfer
	campaignTransfer CampaignType = "CampaignTransfer"
)

const noLimit = math.MaxUint64

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// lockedRand is a small wrapper around rand.Rand to provide
// synchronization among multiple raft groups. Only the methods needed
// by the code are exposed (e.g. Intn).
type lockedRand struct {
	mu sync.Mutex
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	r.mu.Unlock()
	return int(v.Int64())
}

var globalRand = &lockedRand{}

// CampaignType represents the type of campaigning
// the reason we use the type of string instead of uint64
// is because it's simpler to compare and fill in raft entries
type CampaignType string

// StateType represents the role of a node in a cluster.
type StateType uint64

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
	"StatePreCandidate",
}

func (st StateType) String() string {
	return stmap[st]
}

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID pb.PeerID

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64

	// AsyncStorageWrites configures the raft node to write to its local storage
	// (raft log and state machine) using a request/response message passing
	// interface instead of the default Ready/Advance function call interface.
	// Local storage messages can be pipelined and processed asynchronously
	// (with respect to Ready iteration), facilitating reduced interference
	// between Raft proposals and increased batching of log appends and state
	// machine application. As a result, use of asynchronous storage writes can
	// reduce end-to-end commit latency and increase maximum throughput.
	//
	// When true, the Ready.Message slice will include MsgStorageAppend and
	// MsgStorageApply messages. The messages will target a LocalAppendThread
	// and a LocalApplyThread, respectively. Messages to the same target must be
	// reliably processed in order. In other words, they can't be dropped (like
	// messages over the network) and those targeted at the same thread can't be
	// reordered. Messages to different targets can be processed in any order.
	//
	// MsgStorageAppend carries Raft log entries to append, election votes /
	// term changes / updated commit indexes to persist, and snapshots to apply.
	// All writes performed in service of a MsgStorageAppend must be durable
	// before response messages are delivered. However, if the MsgStorageAppend
	// carries no response messages, durability is not required. The message
	// assumes the role of the Entries, HardState, and Snapshot fields in Ready.
	//
	// MsgStorageApply carries committed entries to apply. Writes performed in
	// service of a MsgStorageApply need not be durable before response messages
	// are delivered. The message assumes the role of the CommittedEntries field
	// in Ready.
	//
	// Local messages each carry one or more response messages which should be
	// delivered after the corresponding storage write has been completed. These
	// responses may target the same node or may target other nodes. The storage
	// threads are not responsible for understanding the response messages, only
	// for delivering them to the correct target after performing the storage
	// write.
	AsyncStorageWrites bool

	// MaxSizePerMsg limits the max byte size of each append message. Smaller
	// value lowers the raft recovery cost(initial probing and message lost
	// during normal operation). On the other side, it might affect the
	// throughput during normal replication. Note: math.MaxUint64 for unlimited,
	// 0 for at most one entry per message.
	MaxSizePerMsg uint64
	// MaxCommittedSizePerReady limits the size of the committed entries which
	// can be applying at the same time.
	//
	// Despite its name (preserved for compatibility), this quota applies across
	// Ready structs to encompass all outstanding entries in unacknowledged
	// MsgStorageApply messages when AsyncStorageWrites is enabled.
	MaxCommittedSizePerReady uint64
	// MaxUncommittedEntriesSize limits the aggregate byte size of the
	// uncommitted entries that may be appended to a leader's log. Once this
	// limit is exceeded, proposals will begin to return ErrProposalDropped
	// errors. Note: 0 for no limit.
	MaxUncommittedEntriesSize uint64
	// MaxInflightMsgs limits the max number of in-flight append messages during
	// optimistic replication phase. The application transportation layer usually
	// has its own sending buffer over TCP/UDP. Setting MaxInflightMsgs to avoid
	// overflowing that sending buffer. TODO (xiangli): feedback to application to
	// limit the proposal rate?
	MaxInflightMsgs int
	// MaxInflightBytes limits the number of in-flight bytes in append messages.
	// Complements MaxInflightMsgs. Ignored if zero.
	//
	// This effectively bounds the bandwidth-delay product. Note that especially
	// in high-latency deployments setting this too low can lead to a dramatic
	// reduction in throughput. For example, with a peer that has a round-trip
	// latency of 100ms to the leader and this setting is set to 1 MB, there is a
	// throughput limit of 10 MB/s for this group. With RTT of 400ms, this drops
	// to 2.5 MB/s. See Little's law to understand the maths behind.
	MaxInflightBytes uint64

	// CheckQuorum specifies if the leader should check quorum activity. Leader
	// steps down when quorum is not active for an electionTimeout.
	CheckQuorum bool

	// PreVote enables the Pre-Vote algorithm described in raft thesis section
	// 9.6. This prevents disruption when a node that has been partitioned away
	// rejoins the cluster.
	PreVote bool

	// Logger is the logger used for raft log. For multinode which can host
	// multiple raft group, each raft group can have its own logger
	Logger Logger

	// DisableProposalForwarding set to true means that followers will drop
	// proposals, rather than forwarding them to the leader. One use case for
	// this feature would be in a situation where the Raft leader is used to
	// compute the data of a proposal, for example, adding a timestamp from a
	// hybrid logical clock to data in a monotonically increasing way. Forwarding
	// should be disabled to prevent a follower with an inaccurate hybrid
	// logical clock from assigning the timestamp and then forwarding the data
	// to the leader.
	DisableProposalForwarding bool

	// DisableConfChangeValidation turns off propose-time verification of
	// configuration changes against the currently active configuration of the
	// raft instance. These checks are generally sensible (cannot leave a joint
	// config unless in a joint config, et cetera) but they have false positives
	// because the active configuration may not be the most recent
	// configuration. This is because configurations are activated during log
	// application, and even the leader can trail log application by an
	// unbounded number of entries.
	// Symmetrically, the mechanism has false negatives - because the check may
	// not run against the "actual" config that will be the predecessor of the
	// newly proposed one, the check may pass but the new config may be invalid
	// when it is being applied. In other words, the checks are best-effort.
	//
	// Users should *not* use this option unless they have a reliable mechanism
	// (above raft) that serializes and verifies configuration changes. If an
	// invalid configuration change enters the log and gets applied, a panic
	// will result.
	//
	// This option may be removed once false positives are no longer possible.
	// See: https://github.com/etcd-io/raft/issues/80
	DisableConfChangeValidation bool

	// StepDownOnRemoval makes the leader step down when it is removed from the
	// group or demoted to a learner.
	//
	// This behavior will become unconditional in the future. See:
	// https://github.com/etcd-io/raft/issues/83
	StepDownOnRemoval bool

	// StoreLiveness is a reference to the store liveness fabric.
	StoreLiveness raftstoreliveness.StoreLiveness
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}
	if IsLocalMsgTarget(c.ID) {
		return errors.New("cannot use local target as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	if c.MaxUncommittedEntriesSize == 0 {
		c.MaxUncommittedEntriesSize = noLimit
	}

	// default MaxCommittedSizePerReady to MaxSizePerMsg because they were
	// previously the same parameter.
	if c.MaxCommittedSizePerReady == 0 {
		c.MaxCommittedSizePerReady = c.MaxSizePerMsg
	}

	if c.MaxInflightMsgs <= 0 {
		return errors.New("max inflight messages must be greater than 0")
	}
	if c.MaxInflightBytes == 0 {
		c.MaxInflightBytes = noLimit
	} else if c.MaxInflightBytes < c.MaxSizePerMsg {
		return errors.New("max inflight bytes must be >= max message size")
	}

	if c.Logger == nil {
		c.Logger = getLogger()
	}

	return nil
}

type raft struct {
	id pb.PeerID

	Term uint64
	Vote pb.PeerID

	// the log
	raftLog *raftLog

	maxMsgSize         entryEncodingSize
	maxUncommittedSize entryPayloadSize

	config          quorum.Config
	trk             tracker.ProgressTracker
	electionTracker tracker.ElectionTracker

	state StateType

	// isLearner is true if the local raft node is a learner.
	isLearner bool

	// msgs contains the list of messages that should be sent out immediately to
	// other nodes.
	//
	// Messages in this list must target other nodes.
	msgs []pb.Message
	// msgsAfterAppend contains the list of messages that should be sent after
	// the accumulated unstable state (e.g. term, vote, []entry, and snapshot)
	// has been persisted to durable storage. This includes waiting for any
	// unstable state that is already in the process of being persisted (i.e.
	// has already been handed out in a prior Ready struct) to complete.
	//
	// Messages in this list may target other nodes or may target this node.
	//
	// Messages in this list have the type MsgAppResp, MsgVoteResp, or
	// MsgPreVoteResp. See the comment in raft.send for details.
	msgsAfterAppend []pb.Message

	// the leader id
	lead pb.PeerID
	// TODO(arul): This should be populated when responding to a MsgFortify.
	leadEpoch raftstoreliveness.Epoch
	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in raft thesis 3.10.
	leadTransferee pb.PeerID
	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via pendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	pendingConfIndex uint64
	// disableConfChangeValidation is Config.DisableConfChangeValidation,
	// see there for details.
	disableConfChangeValidation bool
	// an estimate of the size of the uncommitted tail of the Raft log. Used to
	// prevent unbounded log growth. Only maintained by the leader. Reset on
	// term changes.
	uncommittedSize entryPayloadSize

	// number of ticks since it reached last electionTimeout when it is leader
	// or candidate.
	// number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int

	maxInflight      int
	maxInflightBytes uint64
	checkQuorum      bool
	preVote          bool

	heartbeatTimeout int
	electionTimeout  int
	// randomizedElectionTimeout is a random number between
	// [electiontimeout, 2 * electiontimeout - 1]. It gets reset
	// when raft changes its state to follower or candidate.
	randomizedElectionTimeout int
	disableProposalForwarding bool
	stepDownOnRemoval         bool

	tick func()
	step stepFunc

	logger        Logger
	storeLiveness raftstoreliveness.StoreLiveness
}

func newRaft(c *Config) *raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	raftlog := newLogWithSize(c.Storage, c.Logger, entryEncodingSize(c.MaxCommittedSizePerReady))
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}

	r := &raft{
		id:                          c.ID,
		lead:                        None,
		isLearner:                   false,
		raftLog:                     raftlog,
		maxMsgSize:                  entryEncodingSize(c.MaxSizePerMsg),
		maxUncommittedSize:          entryPayloadSize(c.MaxUncommittedEntriesSize),
		electionTimeout:             c.ElectionTick,
		heartbeatTimeout:            c.HeartbeatTick,
		logger:                      c.Logger,
		maxInflight:                 c.MaxInflightMsgs,
		maxInflightBytes:            c.MaxInflightBytes,
		checkQuorum:                 c.CheckQuorum,
		preVote:                     c.PreVote,
		disableProposalForwarding:   c.DisableProposalForwarding,
		disableConfChangeValidation: c.DisableConfChangeValidation,
		stepDownOnRemoval:           c.StepDownOnRemoval,
		storeLiveness:               c.StoreLiveness,
	}
	lastID := r.raftLog.lastEntryID()

	r.electionTracker = tracker.MakeVoteTracker(&r.config)

	cfg, progressMap, err := confchange.Restore(confchange.Changer{
		Config:           quorum.MakeEmptyConfig(),
		ProgressMap:      tracker.MakeEmptyProgressMap(),
		MaxInflight:      r.maxInflight,
		MaxInflightBytes: r.maxInflightBytes,
		LastIndex:        lastID.index,
	}, cs)
	if err != nil {
		panic(err)
	}
	assertConfStatesEquivalent(r.logger, cs, r.switchToConfig(cfg, progressMap))

	if !IsEmptyHardState(hs) {
		r.loadState(hs)
	}
	if c.Applied > 0 {
		raftlog.appliedTo(c.Applied, 0 /* size */)
	}
	r.becomeFollower(r.Term, r.lead)

	var nodesStrs []string
	for _, n := range r.trk.VoterNodes() {
		nodesStrs = append(nodesStrs, fmt.Sprintf("%x", n))
	}

	// TODO(pav-kv): it should be ok to simply print %+v for lastID.
	r.logger.Infof("newRaft %x [peers: [%s], term: %d, commit: %d, applied: %d, lastindex: %d, lastterm: %d]",
		r.id, strings.Join(nodesStrs, ","), r.Term, r.raftLog.committed, r.raftLog.applied, lastID.index, lastID.term)
	return r
}

func (r *raft) hasLeader() bool { return r.lead != None }

func (r *raft) softState() SoftState { return SoftState{RaftState: r.state} }

func (r *raft) hardState() pb.HardState {
	return pb.HardState{
		Term:      r.Term,
		Vote:      r.Vote,
		Commit:    r.raftLog.committed,
		Lead:      r.lead,
		LeadEpoch: r.leadEpoch,
	}
}

// send schedules persisting state to a stable storage and AFTER that
// sending the message (as part of next Ready message processing).
func (r *raft) send(m pb.Message) {
	if m.From == None {
		m.From = r.id
	}
	if m.Type == pb.MsgVote || m.Type == pb.MsgVoteResp || m.Type == pb.MsgPreVote || m.Type == pb.MsgPreVoteResp {
		if m.Term == 0 {
			// All {pre-,}campaign messages need to have the term set when
			// sending.
			// - MsgVote: m.Term is the term the node is campaigning for,
			//   non-zero as we increment the term when campaigning.
			// - MsgVoteResp: m.Term is the new r.Term if the MsgVote was
			//   granted, non-zero for the same reason MsgVote is
			// - MsgPreVote: m.Term is the term the node will campaign,
			//   non-zero as we use m.Term to indicate the next term we'll be
			//   campaigning for
			// - MsgPreVoteResp: m.Term is the term received in the original
			//   MsgPreVote if the pre-vote was granted, non-zero for the
			//   same reasons MsgPreVote is
			r.logger.Panicf("term should be set when sending %s", m.Type)
		}
	} else {
		if m.Term != 0 {
			r.logger.Panicf("term should not be set when sending %s (was %d)", m.Type, m.Term)
		}
		// Do not attach term to MsgProp. Proposals are a way to forward to the
		// leader, and should be treated as local message.
		if m.Type != pb.MsgProp {
			m.Term = r.Term
		}
	}
	if m.Type == pb.MsgAppResp || m.Type == pb.MsgVoteResp || m.Type == pb.MsgPreVoteResp {
		// If async storage writes are enabled, messages added to the msgs slice
		// are allowed to be sent out before unstable state (e.g. log entry
		// writes and election votes) have been durably synced to the local
		// disk.
		//
		// For most message types, this is not an issue. However, response
		// messages that relate to "voting" on either leader election or log
		// appends require durability before they can be sent. It would be
		// incorrect to publish a vote in an election before that vote has been
		// synced to stable storage locally. Similarly, it would be incorrect to
		// acknowledge a log append to the leader before that entry has been
		// synced to stable storage locally.
		//
		// Per the Raft thesis, section 3.8 Persisted state and server restarts:
		//
		// > Raft servers must persist enough information to stable storage to
		// > survive server restarts safely. In particular, each server persists
		// > its current term and vote; this is necessary to prevent the server
		// > from voting twice in the same term or replacing log entries from a
		// > newer leader with those from a deposed leader. Each server also
		// > persists new log entries before they are counted towards the entries’
		// > commitment; this prevents committed entries from being lost or
		// > “uncommitted” when servers restart
		//
		// To enforce this durability requirement, these response messages are
		// queued to be sent out as soon as the current collection of unstable
		// state (the state that the response message was predicated upon) has
		// been durably persisted. This unstable state may have already been
		// passed to a Ready struct whose persistence is in progress or may be
		// waiting for the next Ready struct to begin being written to Storage.
		// These messages must wait for all of this state to be durable before
		// being published.
		//
		// Rejected responses (m.Reject == true) present an interesting case
		// where the durability requirement is less unambiguous. A rejection may
		// be predicated upon unstable state. For instance, a node may reject a
		// vote for one peer because it has already begun syncing its vote for
		// another peer. Or it may reject a vote from one peer because it has
		// unstable log entries that indicate that the peer is behind on its
		// log. In these cases, it is likely safe to send out the rejection
		// response immediately without compromising safety in the presence of a
		// server restart. However, because these rejections are rare and
		// because the safety of such behavior has not been formally verified,
		// we err on the side of safety and omit a `&& !m.Reject` condition
		// above.
		r.msgsAfterAppend = append(r.msgsAfterAppend, m)
	} else {
		if m.To == r.id {
			r.logger.Panicf("message should not be self-addressed when sending %s", m.Type)
		}
		r.msgs = append(r.msgs, m)
	}
}

// maybeSendAppend sends an append RPC with log entries (if any) that are not
// yet known to be replicated in the given peer's log, as well as the current
// commit index. Usually it sends a MsgApp message, but in some cases (e.g. the
// log has been compacted) it can send a MsgSnap.
//
// In some cases, the MsgApp message can have zero entries, and yet be sent.
// When the follower log is not fully up-to-date, we must send a MsgApp
// periodically so that eventually the flow is either accepted or rejected. Not
// doing so can result in replication stall, in cases when a MsgApp is dropped.
//
// Returns true if a message was sent, or false otherwise. A message is not sent
// if the follower log and commit index are up-to-date, the flow is paused (for
// reasons like in-flight limits), or the message could not be constructed.
func (r *raft) maybeSendAppend(to pb.PeerID) bool {
	pr := r.trk.Progress(to)

	last, commit := r.raftLog.lastIndex(), r.raftLog.committed
	if !pr.ShouldSendMsgApp(last, commit) {
		return false
	}

	prevIndex := pr.Next - 1
	prevTerm, err := r.raftLog.term(prevIndex)
	if err != nil {
		// The log probably got truncated at >= pr.Next, so we can't catch up the
		// follower log anymore. Send a snapshot instead.
		return r.maybeSendSnapshot(to, pr)
	}

	var entries []pb.Entry
	if pr.CanSendEntries(last) {
		if entries, err = r.raftLog.entries(pr.Next, r.maxMsgSize); err != nil {
			// Send a snapshot if we failed to get the entries.
			return r.maybeSendSnapshot(to, pr)
		}
	}

	// Send the MsgApp, and update the progress accordingly.
	r.send(pb.Message{
		To:      to,
		Type:    pb.MsgApp,
		Index:   prevIndex,
		LogTerm: prevTerm,
		Entries: entries,
		Commit:  commit,
		Match:   pr.Match,
	})
	pr.SentEntries(len(entries), uint64(payloadsSize(entries)))
	pr.SentCommit(commit)
	return true
}

// maybeSendSnapshot fetches a snapshot from Storage, and sends it to the given
// node. Returns true iff the snapshot message has been emitted successfully.
func (r *raft) maybeSendSnapshot(to pb.PeerID, pr *tracker.Progress) bool {
	if !pr.RecentActive {
		r.logger.Debugf("ignore sending snapshot to %x since it is not recently active", to)
		return false
	}

	snapshot, err := r.raftLog.snapshot()
	if err != nil {
		if err == ErrSnapshotTemporarilyUnavailable {
			r.logger.Debugf("%x failed to send snapshot to %x because snapshot is temporarily unavailable", r.id, to)
			return false
		}
		panic(err) // TODO(bdarnell)
	}
	if IsEmptySnap(snapshot) {
		panic("need non-empty snapshot")
	}
	sindex, sterm := snapshot.Metadata.Index, snapshot.Metadata.Term
	r.logger.Debugf("%x [firstindex: %d, commit: %d] sent snapshot[index: %d, term: %d] to %x [%s]",
		r.id, r.raftLog.firstIndex(), r.raftLog.committed, sindex, sterm, to, pr)
	pr.BecomeSnapshot(sindex)
	r.logger.Debugf("%x paused sending replication messages to %x [%s]", r.id, to, pr)

	r.send(pb.Message{To: to, Type: pb.MsgSnap, Snapshot: &snapshot})
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *raft) sendHeartbeat(to pb.PeerID) {
	pr := r.trk.Progress(to)
	// Attach the commit as min(to.matched, r.committed).
	// When the leader sends out heartbeat message,
	// the receiver(follower) might not be matched with the leader
	// or it might not have all the committed entries.
	// The leader MUST NOT forward the follower's commit to
	// an unmatched index.
	commit := min(pr.Match, r.raftLog.committed)
	r.send(pb.Message{
		To:     to,
		Type:   pb.MsgHeartbeat,
		Commit: commit,
		Match:  pr.Match,
	})
	pr.SentCommit(commit)
}

// bcastAppend sends RPC, with entries to all peers that are not up-to-date
// according to the progress recorded in r.trk.
func (r *raft) bcastAppend() {
	r.trk.Visit(func(id pb.PeerID, _ *tracker.Progress) {
		if id == r.id {
			return
		}
		r.maybeSendAppend(id)
	})
}

// bcastHeartbeat sends RPC, without entries to all the peers.
func (r *raft) bcastHeartbeat() {
	r.trk.Visit(func(id pb.PeerID, _ *tracker.Progress) {
		if id == r.id {
			return
		}
		r.sendHeartbeat(id)
	})
}

func (r *raft) appliedTo(index uint64, size entryEncodingSize) {
	oldApplied := r.raftLog.applied
	newApplied := max(index, oldApplied)
	r.raftLog.appliedTo(newApplied, size)

	if r.config.AutoLeave && newApplied >= r.pendingConfIndex && r.state == StateLeader {
		// If the current (and most recent, at least for this leader's term)
		// configuration should be auto-left, initiate that now. We use a
		// nil Data which unmarshals into an empty ConfChangeV2 and has the
		// benefit that appendEntry can never refuse it based on its size
		// (which registers as zero).
		m, err := confChangeToMsg(nil)
		if err != nil {
			panic(err)
		}
		// NB: this proposal can't be dropped due to size, but can be
		// dropped if a leadership transfer is in progress. We'll keep
		// checking this condition on each applied entry, so either the
		// leadership transfer will succeed and the new leader will leave
		// the joint configuration, or the leadership transfer will fail,
		// and we will propose the config change on the next advance.
		if err := r.Step(m); err != nil {
			r.logger.Debugf("not initiating automatic transition out of joint configuration %s: %v", r.config, err)
		} else {
			r.logger.Infof("initiating automatic transition out of joint configuration %s", r.config)
		}
	}
}

func (r *raft) appliedSnap(snap *pb.Snapshot) {
	index := snap.Metadata.Index
	r.raftLog.stableSnapTo(index)
	r.appliedTo(index, 0 /* size */)
}

// maybeCommit attempts to advance the commit index. Returns true if the commit
// index changed (in which case the caller should call r.bcastAppend). This can
// only be called in StateLeader.
func (r *raft) maybeCommit() bool {
	index := r.trk.Committed()
	if index <= r.raftLog.committed {
		// The commit index must not regress.
		// TODO(pav-kv): consider making it an assertion.
		return false
	}
	// Section 5.4.2 of raft paper (https://raft.github.io/raft.pdf):
	//
	// Only log entries from the leader’s current term are committed by counting
	// replicas; once an entry from the current term has been committed in this
	// way, then all prior entries are committed indirectly because of the Log
	// Matching Property.
	if !r.raftLog.matchTerm(entryID{term: r.Term, index: index}) {
		return false
	}
	r.raftLog.commitTo(logMark{term: r.Term, index: index})
	return true
}

func (r *raft) reset(term uint64) {
	if r.Term != term {
		r.Term = term
		r.Vote = None
	}

	// TODO(arul): we should only reset this if the term has changed.
	r.lead = None

	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetRandomizedElectionTimeout()

	r.abortLeaderTransfer()

	r.electionTracker.ResetVotes()
	r.trk.Visit(func(id pb.PeerID, pr *tracker.Progress) {
		*pr = tracker.Progress{
			Match:     0,
			Next:      r.raftLog.lastIndex() + 1,
			Inflights: tracker.NewInflights(r.maxInflight, r.maxInflightBytes),
			IsLearner: pr.IsLearner,
		}
		if id == r.id {
			pr.Match = r.raftLog.lastIndex()
		}
	})

	r.pendingConfIndex = 0
	r.uncommittedSize = 0
}

func (r *raft) appendEntry(es ...pb.Entry) (accepted bool) {
	last := r.raftLog.lastEntryID()
	for i := range es {
		es[i].Term = r.Term
		es[i].Index = last.index + 1 + uint64(i)
	}
	// Track the size of this uncommitted proposal.
	if !r.increaseUncommittedSize(es) {
		r.logger.Warningf(
			"%x appending new entries to log would exceed uncommitted entry size limit; dropping proposal",
			r.id,
		)
		// Drop the proposal.
		return false
	}
	app := logSlice{term: r.Term, prev: last, entries: es}
	if err := app.valid(); err != nil {
		r.logger.Panicf("%x leader could not append to its log: %v", r.id, err)
	} else if !r.raftLog.append(app) {
		r.logger.Panicf("%x leader could not append to its log", r.id)
	}

	// On appending entries, the leader is effectively "sending" a MsgApp to its
	// local "acceptor". Since we don't actually send this self-MsgApp, update the
	// progress here as if it was sent.
	r.trk.Progress(r.id).Next = app.lastIndex() + 1
	// The leader needs to self-ack the entries just appended once they have
	// been durably persisted (since it doesn't send an MsgApp to itself). This
	// response message will be added to msgsAfterAppend and delivered back to
	// this node after these entries have been written to stable storage. When
	// handled, this is roughly equivalent to:
	//
	//  r.trk.Progress[r.id].MaybeUpdate(e.Index)
	//  if r.maybeCommit() {
	//  	r.bcastAppend()
	//  }
	r.send(pb.Message{To: r.id, Type: pb.MsgAppResp, Index: r.raftLog.lastIndex()})
	return true
}

// tickElection is run by followers and candidates after r.electionTimeout.
func (r *raft) tickElection() {
	r.electionElapsed++

	if r.promotable() && r.pastElectionTimeout() {
		r.electionElapsed = 0
		if err := r.Step(pb.Message{From: r.id, Type: pb.MsgHup}); err != nil {
			r.logger.Debugf("error occurred during election: %v", err)
		}
	}
}

// tickHeartbeat is run by leaders to send a MsgBeat after r.heartbeatTimeout.
func (r *raft) tickHeartbeat() {
	r.heartbeatElapsed++
	r.electionElapsed++

	if r.electionElapsed >= r.electionTimeout {
		r.electionElapsed = 0
		if r.checkQuorum {
			if err := r.Step(pb.Message{From: r.id, Type: pb.MsgCheckQuorum}); err != nil {
				r.logger.Debugf("error occurred during checking sending heartbeat: %v", err)
			}
		}
		// If current leader cannot transfer leadership in electionTimeout, it becomes leader again.
		if r.state == StateLeader && r.leadTransferee != None {
			r.abortLeaderTransfer()
		}
	}

	if r.state != StateLeader {
		return
	}

	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		if err := r.Step(pb.Message{From: r.id, Type: pb.MsgBeat}); err != nil {
			r.logger.Debugf("error occurred during checking sending heartbeat: %v", err)
		}
	}
}

// TODO(arul): Consider removing the lead argument from this function. Instead,
// for all the methods that want to set the leader explicitly (the ones that are
// passing in m.From for this field), we can instead have them use an assignLead
// function instead; in there, we can add safety checks to ensure we're not
// overwriting the leader.
func (r *raft) becomeFollower(term uint64, lead pb.PeerID) {
	r.step = stepFollower
	r.reset(term)
	r.tick = r.tickElection
	r.lead = lead
	r.state = StateFollower
	r.logger.Infof("%x became follower at term %d", r.id, r.Term)
}

func (r *raft) becomeCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> candidate]")
	}
	r.step = stepCandidate
	r.reset(r.Term + 1)
	r.tick = r.tickElection
	r.Vote = r.id
	r.state = StateCandidate
	r.logger.Infof("%x became candidate at term %d", r.id, r.Term)
}

func (r *raft) becomePreCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> pre-candidate]")
	}
	// Becoming a pre-candidate changes our step functions and state,
	// but doesn't change anything else. In particular it does not increase
	// r.Term or change r.Vote.
	r.step = stepCandidate
	r.electionTracker.ResetVotes()
	r.tick = r.tickElection
	// TODO(arul): We're forgetting the raft leader here. From the perspective of
	// leader leases, this is fine, because we wouldn't be here unless we'd
	// revoked StoreLiveness support for the leader's store to begin with. It's
	// a bit weird from the perspective of raft though. See if we can avoid this.
	r.lead = None
	r.state = StatePreCandidate
	r.logger.Infof("%x became pre-candidate at term %d", r.id, r.Term)
}

func (r *raft) becomeLeader() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateFollower {
		panic("invalid transition [follower -> leader]")
	}
	r.step = stepLeader
	r.reset(r.Term)
	r.tick = r.tickHeartbeat
	r.lead = r.id
	r.state = StateLeader
	// Followers enter replicate mode when they've been successfully probed
	// (perhaps after having received a snapshot as a result). The leader is
	// trivially in this state. Note that r.reset() has initialized this
	// progress with the last index already.
	pr := r.trk.Progress(r.id)
	pr.BecomeReplicate()
	// The leader always has RecentActive == true; MsgCheckQuorum makes sure to
	// preserve this.
	pr.RecentActive = true

	// Conservatively set the pendingConfIndex to the last index in the
	// log. There may or may not be a pending config change, but it's
	// safe to delay any future proposals until we commit all our
	// pending log entries, and scanning the entire tail of the log
	// could be expensive.
	r.pendingConfIndex = r.raftLog.lastIndex()

	emptyEnt := pb.Entry{Data: nil}
	if !r.appendEntry(emptyEnt) {
		// This won't happen because we just called reset() above.
		r.logger.Panic("empty entry was dropped")
	}
	// The payloadSize of an empty entry is 0 (see TestPayloadSizeOfEmptyEntry),
	// so the preceding log append does not count against the uncommitted log
	// quota of the new leader. In other words, after the call to appendEntry,
	// r.uncommittedSize is still 0.
	r.logger.Infof("%x became leader at term %d", r.id, r.Term)
}

func (r *raft) hup(t CampaignType) {
	if r.state == StateLeader {
		r.logger.Debugf("%x ignoring MsgHup because already leader", r.id)
		return
	}

	if !r.promotable() {
		r.logger.Warningf("%x is unpromotable and can not campaign", r.id)
		return
	}
	if r.hasUnappliedConfChanges() {
		r.logger.Warningf("%x cannot campaign at term %d since there are still pending configuration changes to apply", r.id, r.Term)
		return
	}

	r.logger.Infof("%x is starting a new election at term %d", r.id, r.Term)
	r.campaign(t)
}

// errBreak is a sentinel error used to break a callback-based loop.
var errBreak = errors.New("break")

func (r *raft) hasUnappliedConfChanges() bool {
	if r.raftLog.applied >= r.raftLog.committed { // in fact applied == committed
		return false
	}
	found := false
	// Scan all unapplied committed entries to find a config change. Paginate the
	// scan, to avoid a potentially unlimited memory spike.
	lo, hi := r.raftLog.applied+1, r.raftLog.committed+1
	// Reuse the maxApplyingEntsSize limit because it is used for similar purposes
	// (limiting the read of unapplied committed entries) when raft sends entries
	// via the Ready struct for application.
	// TODO(pavelkalinnikov): find a way to budget memory/bandwidth for this scan
	// outside the raft package.
	pageSize := r.raftLog.maxApplyingEntsSize
	if err := r.raftLog.scan(lo, hi, pageSize, func(ents []pb.Entry) error {
		for i := range ents {
			if ents[i].Type == pb.EntryConfChange || ents[i].Type == pb.EntryConfChangeV2 {
				found = true
				return errBreak
			}
		}
		return nil
	}); err != nil && err != errBreak {
		r.logger.Panicf("error scanning unapplied entries [%d, %d): %v", lo, hi, err)
	}
	return found
}

// campaign transitions the raft instance to candidate state. This must only be
// called after verifying that this is a legitimate transition.
func (r *raft) campaign(t CampaignType) {
	if !r.promotable() {
		// This path should not be hit (callers are supposed to check), but
		// better safe than sorry.
		r.logger.Warningf("%x is unpromotable; campaign() should have been called", r.id)
	}
	var term uint64
	var voteMsg pb.MessageType
	if t == campaignPreElection {
		r.becomePreCandidate()
		voteMsg = pb.MsgPreVote
		// PreVote RPCs are sent for the next term before we've incremented r.Term.
		term = r.Term + 1
	} else {
		r.becomeCandidate()
		voteMsg = pb.MsgVote
		term = r.Term
	}
	var ids []pb.PeerID
	{
		idMap := r.config.Voters.IDs()
		ids = make([]pb.PeerID, 0, len(idMap))
		for id := range idMap {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	for _, id := range ids {
		if id == r.id {
			// The candidate votes for itself and should account for this self
			// vote once the vote has been durably persisted (since it doesn't
			// send a MsgVote to itself). This response message will be added to
			// msgsAfterAppend and delivered back to this node after the vote
			// has been written to stable storage.
			r.send(pb.Message{To: id, Term: term, Type: voteRespMsgType(voteMsg)})
			continue
		}
		// TODO(pav-kv): it should be ok to simply print %+v for the lastEntryID.
		last := r.raftLog.lastEntryID()
		r.logger.Infof("%x [logterm: %d, index: %d] sent %s request to %x at term %d",
			r.id, last.term, last.index, voteMsg, id, r.Term)

		var ctx []byte
		if t == campaignTransfer {
			ctx = []byte(t)
		}
		r.send(pb.Message{To: id, Term: term, Type: voteMsg, Index: last.index, LogTerm: last.term, Context: ctx})
	}
}

func (r *raft) poll(
	id pb.PeerID, t pb.MessageType, v bool,
) (granted int, rejected int, result quorum.VoteResult) {
	if v {
		r.logger.Infof("%x received %s from %x at term %d", r.id, t, id, r.Term)
	} else {
		r.logger.Infof("%x received %s rejection from %x at term %d", r.id, t, id, r.Term)
	}
	r.electionTracker.RecordVote(id, v)
	return r.electionTracker.TallyVotes()
}

func (r *raft) Step(m pb.Message) error {
	// Handle the message term, which may result in our stepping down to a follower.
	switch {
	case m.Term == 0:
		// local message
	case m.Term > r.Term:
		if m.Type == pb.MsgVote || m.Type == pb.MsgPreVote {
			force := bytes.Equal(m.Context, []byte(campaignTransfer))
			inLease := r.checkQuorum && r.lead != None && r.electionElapsed < r.electionTimeout
			if !force && inLease {
				// If a server receives a RequestVote request within the minimum election timeout
				// of hearing from a current leader, it does not update its term or grant its vote
				last := r.raftLog.lastEntryID()
				// TODO(pav-kv): it should be ok to simply print the %+v of the lastEntryID.
				r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] ignored %s from %x [logterm: %d, index: %d] at term %d: lease is not expired (remaining ticks: %d)",
					r.id, last.term, last.index, r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term, r.electionTimeout-r.electionElapsed)
				return nil
			}
		}
		switch {
		case m.Type == pb.MsgPreVote:
			// Never change our term in response to a PreVote
		case m.Type == pb.MsgPreVoteResp && !m.Reject:
			// We send pre-vote requests with a term in our future. If the
			// pre-vote is granted, we will increment our term when we get a
			// quorum. If it is not, the term comes from the node that
			// rejected our vote so we should become a follower at the new
			// term.
		default:
			r.logger.Infof("%x [term: %d] received a %s message with higher term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
			if m.Type == pb.MsgApp || m.Type == pb.MsgHeartbeat || m.Type == pb.MsgSnap {
				r.becomeFollower(m.Term, m.From)
			} else {
				r.becomeFollower(m.Term, None)
			}
		}

	case m.Term < r.Term:
		if (r.checkQuorum || r.preVote) && (m.Type == pb.MsgHeartbeat || m.Type == pb.MsgApp) {
			// We have received messages from a leader at a lower term. It is possible
			// that these messages were simply delayed in the network, but this could
			// also mean that this node has advanced its term number during a network
			// partition, and it is now unable to either win an election or to rejoin
			// the majority on the old term. If checkQuorum is false, this will be
			// handled by incrementing term numbers in response to MsgVote with a
			// higher term, but if checkQuorum is true we may not advance the term on
			// MsgVote and must generate other messages to advance the term. The net
			// result of these two features is to minimize the disruption caused by
			// nodes that have been removed from the cluster's configuration: a
			// removed node will send MsgVotes (or MsgPreVotes) which will be ignored,
			// but it will not receive MsgApp or MsgHeartbeat, so it will not create
			// disruptive term increases, by notifying leader of this node's activeness.
			// The above comments also true for Pre-Vote
			//
			// When follower gets isolated, it soon starts an election ending
			// up with a higher term than leader, although it won't receive enough
			// votes to win the election. When it regains connectivity, this response
			// with "pb.MsgAppResp" of higher term would force leader to step down.
			// However, this disruption is inevitable to free this stuck node with
			// fresh election. This can be prevented with Pre-Vote phase.
			r.send(pb.Message{To: m.From, Type: pb.MsgAppResp})
		} else if m.Type == pb.MsgPreVote {
			// Before Pre-Vote enable, there may have candidate with higher term,
			// but less log. After update to Pre-Vote, the cluster may deadlock if
			// we drop messages with a lower term.
			last := r.raftLog.lastEntryID()
			// TODO(pav-kv): it should be ok to simply print %+v of the lastEntryID.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, last.term, last.index, r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: pb.MsgPreVoteResp, Reject: true})
		} else {
			// ignore other cases
			r.logger.Infof("%x [term: %d] ignored a %s message with lower term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
		}
		return nil
	}

	switch m.Type {
	case pb.MsgHup:
		if r.preVote {
			r.hup(campaignPreElection)
		} else {
			r.hup(campaignElection)
		}

	case pb.MsgStorageAppendResp:
		// The snapshot precedes the entries. We acknowledge the snapshot first,
		// then the entries, as required by the unstable structure.
		if m.Snapshot != nil {
			r.appliedSnap(m.Snapshot)
		}
		if m.Index != 0 {
			r.raftLog.stableTo(logMark{term: m.LogTerm, index: m.Index})
		}

	case pb.MsgStorageApplyResp:
		if len(m.Entries) > 0 {
			index := m.Entries[len(m.Entries)-1].Index
			r.appliedTo(index, entsSize(m.Entries))
			r.reduceUncommittedSize(payloadsSize(m.Entries))
		}

	case pb.MsgVote, pb.MsgPreVote:
		// We can vote if this is a repeat of a vote we've already cast...
		canVote := r.Vote == m.From ||
			// ...we haven't voted and we don't think there's a leader yet in this term...
			(r.Vote == None && r.lead == None) ||
			// ...or this is a PreVote for a future term...
			(m.Type == pb.MsgPreVote && m.Term > r.Term)
		// ...and we believe the candidate is up to date.
		lastID := r.raftLog.lastEntryID()
		candLastID := entryID{term: m.LogTerm, index: m.Index}
		if canVote && r.raftLog.isUpToDate(candLastID) {
			// Note: it turns out that that learners must be allowed to cast votes.
			// This seems counter- intuitive but is necessary in the situation in which
			// a learner has been promoted (i.e. is now a voter) but has not learned
			// about this yet.
			// For example, consider a group in which id=1 is a learner and id=2 and
			// id=3 are voters. A configuration change promoting 1 can be committed on
			// the quorum `{2,3}` without the config change being appended to the
			// learner's log. If the leader (say 2) fails, there are de facto two
			// voters remaining. Only 3 can win an election (due to its log containing
			// all committed entries), but to do so it will need 1 to vote. But 1
			// considers itself a learner and will continue to do so until 3 has
			// stepped up as leader, replicates the conf change to 1, and 1 applies it.
			// Ultimately, by receiving a request to vote, the learner realizes that
			// the candidate believes it to be a voter, and that it should act
			// accordingly. The candidate's config may be stale, too; but in that case
			// it won't win the election, at least in the absence of the bug discussed
			// in:
			// https://github.com/etcd-io/etcd/issues/7625#issuecomment-488798263.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] cast %s for %x [logterm: %d, index: %d] at term %d",
				r.id, lastID.term, lastID.index, r.Vote, m.Type, m.From, candLastID.term, candLastID.index, r.Term)
			// When responding to Msg{Pre,}Vote messages we include the term
			// from the message, not the local term. To see why, consider the
			// case where a single node was previously partitioned away and
			// it's local term is now out of date. If we include the local term
			// (recall that for pre-votes we don't update the local term), the
			// (pre-)campaigning node on the other end will proceed to ignore
			// the message (it ignores all out of date messages).
			// The term in the original message and current local term are the
			// same in the case of regular votes, but different for pre-votes.
			r.send(pb.Message{To: m.From, Term: m.Term, Type: voteRespMsgType(m.Type)})
			if m.Type == pb.MsgVote {
				// Only record real votes.
				r.electionElapsed = 0
				r.Vote = m.From
			}
		} else {
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, lastID.term, lastID.index, r.Vote, m.Type, m.From, candLastID.term, candLastID.index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: voteRespMsgType(m.Type), Reject: true})
		}

	default:
		err := r.step(r, m)
		if err != nil {
			return err
		}
	}
	return nil
}

type stepFunc func(r *raft, m pb.Message) error

func stepLeader(r *raft, m pb.Message) error {
	// These message types do not require any progress for m.From.
	switch m.Type {
	case pb.MsgBeat:
		r.bcastHeartbeat()
		return nil
	case pb.MsgCheckQuorum:
		if !r.trk.QuorumActive() {
			r.logger.Warningf("%x stepped down to follower since quorum is not active", r.id)
			// NB: Stepping down because of CheckQuorum is a special, in that we know
			// the QSE is in the past. This means that the leader can safely call a
			// new election or vote for a different peer without regressing the QSE.
			// We don't need to/want to give this any special treatment -- instead, we
			// handle this like the general step down case by simply remembering the
			// term/lead information from our stint as the leader.
			r.becomeFollower(r.Term, r.id)
		}
		// Mark everyone (but ourselves) as inactive in preparation for the next
		// CheckQuorum.
		r.trk.Visit(func(id pb.PeerID, pr *tracker.Progress) {
			if id != r.id {
				pr.RecentActive = false
			}
		})
		return nil
	case pb.MsgProp:
		if len(m.Entries) == 0 {
			r.logger.Panicf("%x stepped empty MsgProp", r.id)
		}
		if r.trk.Progress(r.id) == nil {
			// If we are not currently a member of the range (i.e. this node
			// was removed from the configuration while serving as leader),
			// drop any new proposals.
			return ErrProposalDropped
		}
		if r.leadTransferee != None {
			r.logger.Debugf("%x [term %d] transfer leadership to %x is in progress; dropping proposal", r.id, r.Term, r.leadTransferee)
			return ErrProposalDropped
		}

		for i := range m.Entries {
			e := &m.Entries[i]
			var cc pb.ConfChangeI
			if e.Type == pb.EntryConfChange {
				var ccc pb.ConfChange
				if err := ccc.Unmarshal(e.Data); err != nil {
					panic(err)
				}
				cc = ccc
			} else if e.Type == pb.EntryConfChangeV2 {
				var ccc pb.ConfChangeV2
				if err := ccc.Unmarshal(e.Data); err != nil {
					panic(err)
				}
				cc = ccc
			}
			if cc != nil {
				// Per the "Apply" invariant in the config change safety argument[^1],
				// the leader must not append a config change if it hasn't applied all
				// config changes in its log.
				//
				// [^1]: https://github.com/etcd-io/etcd/issues/7625#issuecomment-489232411
				alreadyPending := r.pendingConfIndex > r.raftLog.applied

				alreadyJoint := len(r.config.Voters[1]) > 0
				wantsLeaveJoint := len(cc.AsV2().Changes) == 0

				var failedCheck string
				if alreadyPending {
					failedCheck = fmt.Sprintf("possible unapplied conf change at index %d (applied to %d)", r.pendingConfIndex, r.raftLog.applied)
				} else if alreadyJoint && !wantsLeaveJoint {
					failedCheck = "must transition out of joint config first"
				} else if !alreadyJoint && wantsLeaveJoint {
					failedCheck = "not in joint state; refusing empty conf change"
				}

				// Allow disabling config change constraints that are guaranteed by the
				// upper state machine layer (incorrect ones will apply as no-ops).
				//
				// NB: !alreadyPending requirement is always respected, for safety.
				if alreadyPending || (failedCheck != "" && !r.disableConfChangeValidation) {
					r.logger.Infof("%x ignoring conf change %v at config %s: %s", r.id, cc, r.config, failedCheck)
					m.Entries[i] = pb.Entry{Type: pb.EntryNormal}
				} else {
					r.pendingConfIndex = r.raftLog.lastIndex() + uint64(i) + 1
				}
			}
		}

		if !r.appendEntry(m.Entries...) {
			return ErrProposalDropped
		}
		r.bcastAppend()
		return nil

	case pb.MsgForgetLeader:
		return nil // noop on leader
	}

	// All other message types require a progress for m.From (pr).
	pr := r.trk.Progress(m.From)
	if pr == nil {
		r.logger.Debugf("%x no progress available for %x", r.id, m.From)
		return nil
	}
	switch m.Type {
	case pb.MsgAppResp:
		// NB: this code path is also hit from (*raft).advance, where the leader steps
		// an MsgAppResp to acknowledge the appended entries in the last Ready.

		pr.RecentActive = true

		if m.Reject {
			// RejectHint is the suggested next base entry for appending (i.e.
			// we try to append entry RejectHint+1 next), and LogTerm is the
			// term that the follower has at index RejectHint. Older versions
			// of this library did not populate LogTerm for rejections and it
			// is zero for followers with an empty log.
			//
			// Under normal circumstances, the leader's log is longer than the
			// follower's and the follower's log is a prefix of the leader's
			// (i.e. there is no divergent uncommitted suffix of the log on the
			// follower). In that case, the first probe reveals where the
			// follower's log ends (RejectHint=follower's last index) and the
			// subsequent probe succeeds.
			//
			// However, when networks are partitioned or systems overloaded,
			// large divergent log tails can occur. The naive attempt, probing
			// entry by entry in decreasing order, will be the product of the
			// length of the diverging tails and the network round-trip latency,
			// which can easily result in hours of time spent probing and can
			// even cause outright outages. The probes are thus optimized as
			// described below.
			r.logger.Debugf("%x received MsgAppResp(rejected, hint: (index %d, term %d)) from %x for index %d",
				r.id, m.RejectHint, m.LogTerm, m.From, m.Index)
			nextProbeIdx := m.RejectHint
			if m.LogTerm > 0 {
				// If the follower has an uncommitted log tail, we would end up
				// probing one by one until we hit the common prefix.
				//
				// For example, if the leader has:
				//
				//   idx        1 2 3 4 5 6 7 8 9
				//              -----------------
				//   term (L)   1 3 3 3 5 5 5 5 5
				//   term (F)   1 1 1 1 2 2
				//
				// Then, after sending an append anchored at (idx=9,term=5) we
				// would receive a RejectHint of 6 and LogTerm of 2. Without the
				// code below, we would try an append at index 6, which would
				// fail again.
				//
				// However, looking only at what the leader knows about its own
				// log and the rejection hint, it is clear that a probe at index
				// 6, 5, 4, 3, and 2 must fail as well:
				//
				// For all of these indexes, the leader's log term is larger than
				// the rejection's log term. If a probe at one of these indexes
				// succeeded, its log term at that index would match the leader's,
				// i.e. 3 or 5 in this example. But the follower already told the
				// leader that it is still at term 2 at index 6, and since the
				// log term only ever goes up (within a log), this is a contradiction.
				//
				// At index 1, however, the leader can draw no such conclusion,
				// as its term 1 is not larger than the term 2 from the
				// follower's rejection. We thus probe at 1, which will succeed
				// in this example. In general, with this approach we probe at
				// most once per term found in the leader's log.
				//
				// There is a similar mechanism on the follower (implemented in
				// handleAppendEntries via a call to findConflictByTerm) that is
				// useful if the follower has a large divergent uncommitted log
				// tail[1], as in this example:
				//
				//   idx        1 2 3 4 5 6 7 8 9
				//              -----------------
				//   term (L)   1 3 3 3 3 3 3 3 7
				//   term (F)   1 3 3 4 4 5 5 5 6
				//
				// Naively, the leader would probe at idx=9, receive a rejection
				// revealing the log term of 6 at the follower. Since the leader's
				// term at the previous index is already smaller than 6, the leader-
				// side optimization discussed above is ineffective. The leader thus
				// probes at index 8 and, naively, receives a rejection for the same
				// index and log term 5. Again, the leader optimization does not improve
				// over linear probing as term 5 is above the leader's term 3 for that
				// and many preceding indexes; the leader would have to probe linearly
				// until it would finally hit index 3, where the probe would succeed.
				//
				// Instead, we apply a similar optimization on the follower. When the
				// follower receives the probe at index 8 (log term 3), it concludes
				// that all of the leader's log preceding that index has log terms of
				// 3 or below. The largest index in the follower's log with a log term
				// of 3 or below is index 3. The follower will thus return a rejection
				// for index=3, log term=3 instead. The leader's next probe will then
				// succeed at that index.
				//
				// [1]: more precisely, if the log terms in the large uncommitted
				// tail on the follower are larger than the leader's. At first,
				// it may seem unintuitive that a follower could even have such
				// a large tail, but it can happen:
				//
				// 1. Leader appends (but does not commit) entries 2 and 3, crashes.
				//   idx        1 2 3 4 5 6 7 8 9
				//              -----------------
				//   term (L)   1 2 2     [crashes]
				//   term (F)   1
				//   term (F)   1
				//
				// 2. a follower becomes leader and appends entries at term 3.
				//              -----------------
				//   term (x)   1 2 2     [down]
				//   term (F)   1 3 3 3 3
				//   term (F)   1
				//
				// 3. term 3 leader goes down, term 2 leader returns as term 4
				//    leader. It commits the log & entries at term 4.
				//
				//              -----------------
				//   term (L)   1 2 2 2
				//   term (x)   1 3 3 3 3 [down]
				//   term (F)   1
				//              -----------------
				//   term (L)   1 2 2 2 4 4 4
				//   term (F)   1 3 3 3 3 [gets probed]
				//   term (F)   1 2 2 2 4 4 4
				//
				// 4. the leader will now probe the returning follower at index
				//    7, the rejection points it at the end of the follower's log
				//    which is at a higher log term than the actually committed
				//    log.
				nextProbeIdx, _ = r.raftLog.findConflictByTerm(m.RejectHint, m.LogTerm)
			}
			if pr.MaybeDecrTo(m.Index, nextProbeIdx) {
				r.logger.Debugf("%x decreased progress of %x to [%s]", r.id, m.From, pr)
				if pr.State == tracker.StateReplicate {
					pr.BecomeProbe()
				}
				r.maybeSendAppend(m.From)
			}
		} else {
			// We want to update our tracking if the response updates our
			// matched index or if the response can move a probing peer back
			// into StateReplicate (see heartbeat_rep_recovers_from_probing.txt
			// for an example of the latter case).
			// NB: the same does not make sense for StateSnapshot - if `m.Index`
			// equals pr.Match we know we don't m.Index+1 in our log, so moving
			// back to replicating state is not useful; besides pr.PendingSnapshot
			// would prevent it.
			if pr.MaybeUpdate(m.Index) || (pr.Match == m.Index && pr.State == tracker.StateProbe) {
				switch {
				case pr.State == tracker.StateProbe:
					pr.BecomeReplicate()
				case pr.State == tracker.StateSnapshot && pr.Match+1 >= r.raftLog.firstIndex():
					// Note that we don't take into account PendingSnapshot to
					// enter this branch. No matter at which index a snapshot
					// was actually applied, as long as this allows catching up
					// the follower from the log, we will accept it. This gives
					// systems more flexibility in how they implement snapshots;
					// see the comments on PendingSnapshot.
					r.logger.Debugf("%x recovered from needing snapshot, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
					// Transition back to replicating state via probing state
					// (which takes the snapshot into account). If we didn't
					// move to replicating state, that would only happen with
					// the next round of appends (but there may not be a next
					// round for a while, exposing an inconsistent RaftStatus).
					pr.BecomeProbe()
					pr.BecomeReplicate()
				case pr.State == tracker.StateReplicate:
					pr.Inflights.FreeLE(m.Index)
				}

				if r.maybeCommit() {
					r.bcastAppend()
				}
				// We've updated flow control information above, which may allow us to
				// send multiple (size-limited) in-flight messages at once (such as when
				// transitioning from probe to replicate, or when freeTo() covers
				// multiple messages). Send as many messages as we can.
				if r.id != m.From {
					for r.maybeSendAppend(m.From) {
					}
				}
				// Transfer leadership is in progress.
				if m.From == r.leadTransferee && pr.Match == r.raftLog.lastIndex() {
					r.logger.Infof("%x sent MsgTimeoutNow to %x after received MsgAppResp", r.id, m.From)
					r.sendTimeoutNow(m.From)
				}
			}
		}
	case pb.MsgHeartbeatResp:
		pr.RecentActive = true
		pr.MsgAppProbesPaused = false
		r.maybeSendAppend(m.From)

	case pb.MsgSnapStatus:
		if pr.State != tracker.StateSnapshot {
			return nil
		}
		if !m.Reject {
			pr.BecomeProbe()
			r.logger.Debugf("%x snapshot succeeded, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		} else {
			// NB: the order here matters or we'll be probing erroneously from
			// the snapshot index, but the snapshot never applied.
			pr.PendingSnapshot = 0
			pr.BecomeProbe()
			r.logger.Debugf("%x snapshot failed, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		}
		// If snapshot finish, wait for the MsgAppResp from the remote node before sending
		// out the next MsgApp.
		// If snapshot failure, wait for a heartbeat interval before next try
		pr.MsgAppProbesPaused = true
	case pb.MsgUnreachable:
		// During optimistic replication, if the remote becomes unreachable,
		// there is huge probability that a MsgApp is lost.
		if pr.State == tracker.StateReplicate {
			pr.BecomeProbe()
		}
		r.logger.Debugf("%x failed to send message to %x because it is unreachable [%s]", r.id, m.From, pr)
	case pb.MsgTransferLeader:
		if pr.IsLearner {
			r.logger.Debugf("%x is learner. Ignored transferring leadership", r.id)
			return nil
		}
		leadTransferee := m.From
		lastLeadTransferee := r.leadTransferee
		if lastLeadTransferee != None {
			if lastLeadTransferee == leadTransferee {
				r.logger.Infof("%x [term %d] transfer leadership to %x is in progress, ignores request to same node %x",
					r.id, r.Term, leadTransferee, leadTransferee)
				return nil
			}
			r.abortLeaderTransfer()
			r.logger.Infof("%x [term %d] abort previous transferring leadership to %x", r.id, r.Term, lastLeadTransferee)
		}
		if leadTransferee == r.id {
			r.logger.Debugf("%x is already leader. Ignored transferring leadership to self", r.id)
			return nil
		}
		// Transfer leadership to third party.
		r.logger.Infof("%x [term %d] starts to transfer leadership to %x", r.id, r.Term, leadTransferee)
		// Transfer leadership should be finished in one electionTimeout, so reset r.electionElapsed.
		r.electionElapsed = 0
		r.leadTransferee = leadTransferee
		if pr.Match == r.raftLog.lastIndex() {
			r.sendTimeoutNow(leadTransferee)
			r.logger.Infof("%x sends MsgTimeoutNow to %x immediately as %x already has up-to-date log", r.id, leadTransferee, leadTransferee)
		} else {
			pr.MsgAppProbesPaused = false
			r.maybeSendAppend(leadTransferee)
		}
	}
	return nil
}

// stepCandidate is shared by StateCandidate and StatePreCandidate; the difference is
// whether they respond to MsgVoteResp or MsgPreVoteResp.
func stepCandidate(r *raft, m pb.Message) error {
	// Only handle vote responses corresponding to our candidacy (while in
	// StateCandidate, we may get stale MsgPreVoteResp messages in this term from
	// our pre-candidate state).
	var myVoteRespType pb.MessageType
	if r.state == StatePreCandidate {
		myVoteRespType = pb.MsgPreVoteResp
	} else {
		myVoteRespType = pb.MsgVoteResp
	}
	switch m.Type {
	case pb.MsgProp:
		r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
		return ErrProposalDropped
	case pb.MsgApp:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleAppendEntries(m)
	case pb.MsgHeartbeat:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleHeartbeat(m)
	case pb.MsgSnap:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleSnapshot(m)
	case myVoteRespType:
		gr, rj, res := r.poll(m.From, m.Type, !m.Reject)
		r.logger.Infof("%x has received %d %s votes and %d vote rejections", r.id, gr, m.Type, rj)
		switch res {
		case quorum.VoteWon:
			if r.state == StatePreCandidate {
				r.campaign(campaignElection)
			} else {
				r.becomeLeader()
				r.bcastAppend()
			}
		case quorum.VoteLost:
			// pb.MsgPreVoteResp contains future term of pre-candidate
			// m.Term > r.Term; reuse r.Term
			r.becomeFollower(r.Term, r.lead)
		}
	case pb.MsgTimeoutNow:
		r.logger.Debugf("%x [term %d state %v] ignored MsgTimeoutNow from %x", r.id, r.Term, r.state, m.From)
	}
	return nil
}

func stepFollower(r *raft, m pb.Message) error {
	switch m.Type {
	case pb.MsgProp:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
			return ErrProposalDropped
		} else if r.disableProposalForwarding {
			r.logger.Infof("%x not forwarding to leader %x at term %d; dropping proposal", r.id, r.lead, r.Term)
			return ErrProposalDropped
		} else if r.lead == r.id {
			r.logger.Infof("%x not forwarding to itself at term %d; dropping proposal", r.id, r.Term)
			return ErrProposalDropped
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgApp:
		r.electionElapsed = 0
		// TODO(arul): Once r.lead != None, we shouldn't need to update r.lead
		// anymore within the course of a single term (in the context of which this
		// function is always called). Instead, if r.lead != None, we should be able
		// to assert that the leader hasn't changed within a given term. Maybe at
		// the caller itself.
		r.lead = m.From
		r.handleAppendEntries(m)
	case pb.MsgHeartbeat:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleHeartbeat(m)
	case pb.MsgSnap:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleSnapshot(m)
	case pb.MsgTransferLeader:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping leader transfer msg", r.id, r.Term)
			return nil
		} else if r.lead == r.id {
			r.logger.Infof("%x is itself the leader at term %d; dropping leader transfer msg", r.id, r.Term)
			return nil
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgForgetLeader:
		if r.lead != None {
			r.logger.Infof("%x forgetting leader %x at term %d", r.id, r.lead, r.Term)
			r.lead = None
		}
	case pb.MsgTimeoutNow:
		r.logger.Infof("%x [term %d] received MsgTimeoutNow from %x and starts an election to get leadership.", r.id, r.Term, m.From)
		// Leadership transfers never use pre-vote even if r.preVote is true; we
		// know we are not recovering from a partition so there is no need for the
		// extra round trip.
		r.hup(campaignTransfer)
	}
	return nil
}

// logSliceFromMsgApp extracts the appended logSlice from a MsgApp message.
func logSliceFromMsgApp(m *pb.Message) logSlice {
	// TODO(pav-kv): consider also validating the logSlice here.
	return logSlice{
		term:    m.Term,
		prev:    entryID{term: m.LogTerm, index: m.Index},
		entries: m.Entries,
	}
}

func (r *raft) handleAppendEntries(m pb.Message) {
	r.checkMatch(m.Match)

	// TODO(pav-kv): construct logSlice up the stack next to receiving the
	// message, and validate it before taking any action (e.g. bumping term).
	a := logSliceFromMsgApp(&m)
	if err := a.valid(); err != nil {
		// TODO(pav-kv): add a special kind of logger.Errorf that panics in tests,
		// but logs an error in prod. We want to eliminate all such errors in tests.
		r.logger.Errorf("%x received an invalid MsgApp: %v", r.id, err)
		return
	}

	if a.prev.index < r.raftLog.committed {
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
		return
	}
	if r.raftLog.maybeAppend(a) {
		// TODO(pav-kv): make it possible to commit even if the append did not
		// succeed or is stale. If accTerm >= m.Term, then our log contains all
		// committed entries at m.Term (by raft invariants), so it is safe to bump
		// the commit index even if the MsgApp is stale.
		lastIndex := a.lastIndex()
		r.raftLog.commitTo(logMark{term: m.Term, index: min(m.Commit, lastIndex)})
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: lastIndex})
		return
	}
	r.logger.Debugf("%x [logterm: %d, index: %d] rejected MsgApp [logterm: %d, index: %d] from %x",
		r.id, r.raftLog.zeroTermOnOutOfBounds(r.raftLog.term(m.Index)), m.Index, m.LogTerm, m.Index, m.From)

	// Our log does not match the leader's at index m.Index. Return a hint to the
	// leader - a guess on the maximal (index, term) at which the logs match. Do
	// this by searching through the follower's log for the maximum (index, term)
	// pair with a term <= the MsgApp's LogTerm and an index <= the MsgApp's
	// Index. This can help skip all indexes in the follower's uncommitted tail
	// with terms greater than the MsgApp's LogTerm.
	//
	// See the other caller for findConflictByTerm (in stepLeader) for a much more
	// detailed explanation of this mechanism.

	// NB: m.Index >= raftLog.committed by now (see the early return above), and
	// raftLog.lastIndex() >= raftLog.committed by invariant, so min of the two is
	// also >= raftLog.committed. Hence, the findConflictByTerm argument is within
	// the valid interval, which then will return a valid (index, term) pair with
	// a non-zero term (unless the log is empty). However, it is safe to send a zero
	// LogTerm in this response in any case, so we don't verify it here.
	hintIndex := min(m.Index, r.raftLog.lastIndex())
	hintIndex, hintTerm := r.raftLog.findConflictByTerm(hintIndex, m.LogTerm)
	r.send(pb.Message{
		To:         m.From,
		Type:       pb.MsgAppResp,
		Index:      m.Index,
		Reject:     true,
		RejectHint: hintIndex,
		LogTerm:    hintTerm,
	})
}

// checkMatch ensures that the follower's log size does not contradict to the
// leader's idea where it matches.
func (r *raft) checkMatch(match uint64) {
	// TODO(pav-kv): lastIndex() might be not yet durable. Make this check
	// stronger by comparing `match` with the last durable index.
	//
	// TODO(pav-kv): make this check stronger when the raftLog stores the last
	// accepted term. If `match` is non-zero, this follower's log last accepted
	// term must equal the leader term, and have entries up to `match` durable.
	if last := r.raftLog.lastIndex(); last < match {
		r.logger.Panicf("match(%d) is out of range [lastIndex(%d)]. Was the raft log corrupted, truncated, or lost?", match, last)
	}
}

func (r *raft) handleHeartbeat(m pb.Message) {
	r.checkMatch(m.Match)

	// The m.Term leader is indicating to us through this heartbeat message
	// that indices <= m.Commit in its log are committed. If our log matches
	// the leader's up to index M, then we can update our commit index to
	// min(m.Commit, M).
	//
	// If accTerm == m.Term, i.e. the last accepted log append came from this
	// leader, then we know that our log is a prefix of the leader's log. We can
	// thus put M = r.raftLog.lastIndex() in the formula above.
	//
	// Otherwise (accTerm != m.Term), we haven't accepted a single log append from
	// the m.Term leader, so we don't know M, and it is unsafe to update the
	// commit index.
	//
	// NB: in the latter case, our log is lagging the leader's. If the leader is
	// stable, we will eventually accept a MsgApp which sets accTerm == m.Term and
	// enables advancing the commit index. By this, we have the guarantee that our
	// commit index converges to the leader's.
	//
	// TODO(pav-kv): the condition can be relaxed, it is actually safe to bump the
	// commit index if accTerm >= m.Term.
	// TODO(pav-kv): move this logic to raftLog.commitTo, once the accTerm has
	// migrated to raftLog/unstable.
	mark := logMark{term: m.Term, index: min(m.Commit, r.raftLog.lastIndex())}
	if mark.term == r.raftLog.accTerm() {
		r.raftLog.commitTo(mark)
	}
	r.send(pb.Message{To: m.From, Type: pb.MsgHeartbeatResp})
}

func (r *raft) handleSnapshot(m pb.Message) {
	// MsgSnap messages should always carry a non-nil Snapshot, but err on the
	// side of safety and treat a nil Snapshot as a zero-valued Snapshot.
	s := snapshot{term: m.Term}
	if m.Snapshot != nil {
		s.snap = *m.Snapshot
	}
	if err := s.valid(); err != nil {
		// TODO(pav-kv): add a special kind of logger.Errorf that panics in tests,
		// but logs an error in prod. We want to eliminate all such errors in tests.
		r.logger.Errorf("%x received an invalid MsgSnap: %v", r.id, err)
		return
	}

	id := s.lastEntryID()
	if r.restore(s) {
		r.logger.Infof("%x [commit: %d] restored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, id.index, id.term)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.lastIndex()})
	} else {
		r.logger.Infof("%x [commit: %d] ignored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, id.index, id.term)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
	}
}

// restore recovers the state machine from a snapshot. It restores the log and the
// configuration of state machine. If this method returns false, the snapshot was
// ignored, either because it was obsolete or because of an error.
func (r *raft) restore(s snapshot) bool {
	id := s.lastEntryID()
	if id.index <= r.raftLog.committed {
		return false
	}
	if r.state != StateFollower {
		// This is defense-in-depth: if the leader somehow ended up applying a
		// snapshot, it could move into a new term without moving into a
		// follower state. This should never fire, but if it does, we panic.
		//
		// At the time of writing, the instance is guaranteed to be in follower
		// state when this method is called.
		r.logger.Panicf("%x attempted to restore snapshot in state %s", r.id, r.state)
		return false
	}

	// More defense-in-depth: throw away snapshot if recipient is not in the
	// config. This shouldn't ever happen (at the time of writing) but lots of
	// code here and there assumes that r.id is in the progress tracker.
	found := false
	cs := s.snap.Metadata.ConfState

	for _, set := range [][]pb.PeerID{
		cs.Voters,
		cs.Learners,
		cs.VotersOutgoing,
		// `LearnersNext` doesn't need to be checked. According to the rules, if a peer in
		// `LearnersNext`, it has to be in `VotersOutgoing`.
	} {
		for _, id := range set {
			if id == r.id {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		r.logger.Warningf(
			"%x attempted to restore snapshot but it is not in the ConfState %v; should never happen",
			r.id, cs,
		)
		return false
	}

	// Now go ahead and actually restore.

	if r.raftLog.matchTerm(id) {
		// TODO(pav-kv): can print %+v of the id, but it will change the format.
		last := r.raftLog.lastEntryID()
		r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] fast-forwarded commit to snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, last.index, last.term, id.index, id.term)
		r.raftLog.commitTo(s.mark())
		return false
	}

	if !r.raftLog.restore(s) {
		r.logger.Errorf("%x unable to restore snapshot [index: %d, term: %d]", r.id, id.index, id.term)
		return false
	}

	cfg, progressMap, err := confchange.Restore(confchange.Changer{
		// Reset the configuration and add the (potentially updated) peers in anew.
		Config:           quorum.MakeEmptyConfig(),
		ProgressMap:      tracker.MakeEmptyProgressMap(), // empty ProgressMap to go with our empty config
		MaxInflight:      r.maxInflight,
		MaxInflightBytes: r.maxInflightBytes,
		LastIndex:        r.raftLog.lastIndex(),
	}, cs)

	if err != nil {
		// This should never happen. Either there's a bug in our config change
		// handling or the client corrupted the conf change.
		panic(fmt.Sprintf("unable to restore config %+v: %s", cs, err))
	}

	assertConfStatesEquivalent(r.logger, cs, r.switchToConfig(cfg, progressMap))

	last := r.raftLog.lastEntryID()
	r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] restored snapshot [index: %d, term: %d]",
		r.id, r.raftLog.committed, last.index, last.term, id.index, id.term)
	return true
}

// promotable indicates whether state machine can be promoted to leader,
// which is true when its own id is in progress list.
func (r *raft) promotable() bool {
	pr := r.trk.Progress(r.id)
	return pr != nil && !pr.IsLearner && !r.raftLog.hasNextOrInProgressSnapshot()
}

func (r *raft) applyConfChange(cc pb.ConfChangeV2) pb.ConfState {
	cfg, progressMap, err := func() (quorum.Config, tracker.ProgressMap, error) {
		changer := confchange.Changer{
			Config:           r.config,
			ProgressMap:      r.trk.MoveProgressMap(),
			MaxInflight:      r.maxInflight,
			MaxInflightBytes: r.maxInflightBytes,
			LastIndex:        r.raftLog.lastIndex(),
		}
		if cc.LeaveJoint() {
			return changer.LeaveJoint()
		} else if autoLeave, ok := cc.EnterJoint(); ok {
			return changer.EnterJoint(autoLeave, cc.Changes...)
		}
		return changer.Simple(cc.Changes...)
	}()

	if err != nil {
		// TODO(tbg): return the error to the caller.
		panic(err)
	}

	return r.switchToConfig(cfg, progressMap)
}

// switchToConfig reconfigures this node to use the provided configuration. It
// updates the in-memory state and, when necessary, carries out additional
// actions such as reacting to the removal of nodes or changed quorum
// requirements.
//
// The inputs usually result from restoring a ConfState or applying a ConfChange.
func (r *raft) switchToConfig(cfg quorum.Config, progressMap tracker.ProgressMap) pb.ConfState {
	r.config = cfg
	r.trk = tracker.MakeProgressTracker(&r.config, progressMap)

	r.logger.Infof("%x switched to configuration %s", r.id, r.config)
	cs := r.config.ConfState()
	pr := r.trk.Progress(r.id)

	// Update whether the node itself is a learner, resetting to false when the
	// node is removed.
	r.isLearner = pr != nil && pr.IsLearner

	if (pr == nil || r.isLearner) && r.state == StateLeader {
		// This node is leader and was removed or demoted, step down if requested.
		//
		// We prevent demotions at the time writing but hypothetically we handle
		// them the same way as removing the leader.
		//
		// TODO(tbg): ask follower with largest Match to TimeoutNow (to avoid
		// interruption). This might still drop some proposals but it's better than
		// nothing.
		if r.stepDownOnRemoval {
			// NB: Similar to the CheckQuorum step down case, we must remember our
			// prior stint as leader, lest we regress the QSE.
			r.becomeFollower(r.Term, r.lead)
		}
		return cs
	}

	// The remaining steps only make sense if this node is the leader and there
	// are other nodes.
	if r.state != StateLeader || len(cs.Voters) == 0 {
		return cs
	}

	r.maybeCommit()
	// If the configuration change means that more entries are committed now,
	// broadcast/append to everyone in the updated config.
	//
	// Otherwise, still probe the newly added replicas; there's no reason to let
	// them wait out a heartbeat interval (or the next incoming proposal).
	r.bcastAppend()

	// If the leadTransferee was removed or demoted, abort the leadership transfer.
	if _, tOK := r.config.Voters.IDs()[r.leadTransferee]; !tOK && r.leadTransferee != 0 {
		r.abortLeaderTransfer()
	}

	return cs
}

func (r *raft) loadState(state pb.HardState) {
	if state.Commit < r.raftLog.committed || state.Commit > r.raftLog.lastIndex() {
		r.logger.Panicf("%x state.commit %d is out of range [%d, %d]", r.id, state.Commit, r.raftLog.committed, r.raftLog.lastIndex())
	}
	r.raftLog.committed = state.Commit
	r.Term = state.Term
	r.Vote = state.Vote
	r.lead = state.Lead
	r.leadEpoch = state.LeadEpoch
}

// pastElectionTimeout returns true if r.electionElapsed is greater
// than or equal to the randomized election timeout in
// [electiontimeout, 2 * electiontimeout - 1].
func (r *raft) pastElectionTimeout() bool {
	return r.electionElapsed >= r.randomizedElectionTimeout
}

func (r *raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + globalRand.Intn(r.electionTimeout)
}

func (r *raft) sendTimeoutNow(to pb.PeerID) {
	r.send(pb.Message{To: to, Type: pb.MsgTimeoutNow})
}

func (r *raft) abortLeaderTransfer() {
	r.leadTransferee = None
}

// increaseUncommittedSize computes the size of the proposed entries and
// determines whether they would push leader over its maxUncommittedSize limit.
// If the new entries would exceed the limit, the method returns false. If not,
// the increase in uncommitted entry size is recorded and the method returns
// true.
//
// Empty payloads are never refused. This is used both for appending an empty
// entry at a new leader's term, as well as leaving a joint configuration.
func (r *raft) increaseUncommittedSize(ents []pb.Entry) bool {
	s := payloadsSize(ents)
	if r.uncommittedSize > 0 && s > 0 && r.uncommittedSize+s > r.maxUncommittedSize {
		// If the uncommitted tail of the Raft log is empty, allow any size
		// proposal. Otherwise, limit the size of the uncommitted tail of the
		// log and drop any proposal that would push the size over the limit.
		// Note the added requirement s>0 which is used to make sure that
		// appending single empty entries to the log always succeeds, used both
		// for replicating a new leader's initial empty entry, and for
		// auto-leaving joint configurations.
		return false
	}
	r.uncommittedSize += s
	return true
}

// reduceUncommittedSize accounts for the newly committed entries by decreasing
// the uncommitted entry size limit.
func (r *raft) reduceUncommittedSize(s entryPayloadSize) {
	if s > r.uncommittedSize {
		// uncommittedSize may underestimate the size of the uncommitted Raft
		// log tail but will never overestimate it. Saturate at 0 instead of
		// allowing overflow.
		r.uncommittedSize = 0
	} else {
		r.uncommittedSize -= s
	}
}
