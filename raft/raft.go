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
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"

	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

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
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
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

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// vote counts
	voteCount uint64

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	randomElectionTimeOut int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	raftNode := &Raft{
		id:                    c.ID,
		RaftLog:               newLog(c.Storage),
		Prs:                   make(map[uint64]*Progress),
		votes:                 make(map[uint64]bool),
		voteCount:             0,
		msgs:                  nil,
		electionTimeout:       c.ElectionTick,
		heartbeatTimeout:      c.HeartbeatTick,
		randomElectionTimeOut: c.ElectionTick,
		leadTransferee:        None,
		PendingConfIndex:      None,
	}
	raftNode.becomeFollower(0, None)
	hardState, confState, _ := c.Storage.InitialState()
	raftNode.Vote, raftNode.Term, raftNode.RaftLog.committed = hardState.Vote, hardState.Term, hardState.Commit
	if c.Applied > 0 {
		raftNode.RaftLog.applied = c.Applied
	}
	if c.peers == nil {
		c.peers = confState.Nodes
	}
	if c.peers == nil {
		c.peers = append(c.peers, c.ID)
	}

	lastIndex := raftNode.RaftLog.LastIndex()
	firstIndex, _ := raftNode.RaftLog.storage.FirstIndex()
	for _, prs := range c.peers {
		if prs == raftNode.id {
			raftNode.Prs[prs] = &Progress{Match: lastIndex, Next: lastIndex + 1}
		} else {
			raftNode.Prs[prs] = &Progress{Match: firstIndex - 1, Next: lastIndex + 1}
		}

	}

	return raftNode
}

func (r *Raft) getSoftState() *SoftState {
	return &SoftState{
		Lead:      r.Lead,
		RaftState: r.State,
	}
}

func (r *Raft) getHardState() pb.HardState {
	return pb.HardState{
		Commit: r.RaftLog.committed,
		Term:   r.Term,
		Vote:   r.Vote,
	}
}

func (r *Raft) isSameSoftState(a *SoftState, b *SoftState) bool {
	return a.Lead == b.Lead && a.RaftState == b.RaftState
}

func (r *Raft) isSameHardState(a *pb.HardState, b *pb.HardState) bool {
	return a.Term == b.Term && a.Commit == b.Commit && a.Vote == b.Vote
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	prevIndex := r.Prs[to].Next - 1
	LogTerm, err := r.RaftLog.Term(prevIndex)
	if err != nil {
		if err == ErrCompacted {
			r.sendSnapshot(to)
			return false
		}
		panic(err)
	}

	entries := make([]*pb.Entry, 0)
	for i := r.RaftLog.getPeerIndex(prevIndex + 1); int(i) < len(r.RaftLog.entries); i++ {
		entries = append(entries, &r.RaftLog.entries[i])
	}

	msg := &pb.Message{
		From:    r.id,
		To:      to,
		Term:    r.Term,
		MsgType: pb.MessageType_MsgAppend,
		Entries: entries,
		LogTerm: LogTerm,
		Index:   prevIndex,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, *msg)
	return true
}

func (r *Raft) sendHeartBeatMessage(to uint64) {
	msg := &pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From:    r.id,
		To:      to,
		Term:    r.Term,
	}
	r.msgs = append(r.msgs, *msg)
}

func (r *Raft) sendHeartBeatMessageToPeers() {
	for id := range r.Prs {
		if id != r.id {
			r.sendHeartBeatMessage(id)
		}
	}
}

func (r *Raft) sendHeartBeatResponse(to uint64, reject bool) {
	msg := &pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, *msg)
}

func (r *Raft) sendMessage(msgType pb.MessageType, to uint64) {
	index := r.RaftLog.LastIndex()
	term, _ := r.RaftLog.Term(index)
	msg := &pb.Message{
		MsgType: msgType,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Index:   index,
		LogTerm: term,
	}
	r.msgs = append(r.msgs, *msg)
}

func (r *Raft) sendMessageToPeers(msgType pb.MessageType) {
	for id := range r.Prs {
		if id != r.id {
			r.sendMessage(msgType, id)
		}
	}
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	switch r.State {
	case StateLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			r.heartbeatElapsed = 0
			r.sendHeartBeatMessageToPeers()
		}
	case StateCandidate:
		r.electionElapsed++
		if r.electionElapsed >= r.randomElectionTimeOut {
			r.startElection()
		}
	case StateFollower:
		r.electionElapsed++
		if r.electionElapsed >= r.randomElectionTimeOut {
			r.startElection()
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// log.Infof("become follower: %d, %d", r.id, lead)
	r.Term = term
	r.Lead = lead
	r.heartbeatElapsed = 0
	r.electionElapsed = 0
	r.Vote = lead
	r.State = StateFollower
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// log.Infof("become candidate: %d", r.id)
	r.heartbeatElapsed = 0
	r.electionElapsed = 0
	r.Term++
	r.votes = make(map[uint64]bool)
	r.voteCount = 1
	r.votes[r.id] = true
	r.Vote = r.id
	r.randomElectionTimeOut = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.State = StateCandidate
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	// log.Infof("become leader: %d", r.id)
	r.Lead = r.id
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.State = StateLeader

	lastIndex := r.RaftLog.LastIndex()

	for id := range r.Prs {
		if id == r.id {
			r.Prs[id] = &Progress{Next: lastIndex + 2, Match: lastIndex + 1}
		} else {
			r.Prs[id] = &Progress{Next: lastIndex + 1}
		}
	}
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{Term: r.Term, Index: lastIndex + 1})
	r.broadcastAppendEntries()

	if len(r.Prs) == 1 {
		r.RaftLog.committed = lastIndex + 1
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	if _, ok := r.Prs[r.id]; !ok {
		return ErrStepPeerNotFound
	}
	if m.Term > r.Term {
		r.becomeFollower(m.Term, None)
	}
	// Your Code Here (2A).
	switch r.State {
	case StateFollower:
		r.stepFollower(m)
	case StateCandidate:
		r.stepCandidate(m)
	case StateLeader:
		r.stepLeader(m)
	}
	return nil
}

func (r *Raft) startElection() {
	r.becomeCandidate()
	if r.voteCount > uint64(len(r.Prs)/2) {
		r.becomeLeader()
		return
	}
	r.sendMessageToPeers(pb.MessageType_MsgRequestVote)
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term < r.Term {
		r.sendAppendResponse(m.From, None, None, true)
		return
	}
	if m.Index > r.RaftLog.LastIndex() {
		r.sendAppendResponse(m.From, None, r.RaftLog.LastIndex()+1, true)
		return
	}

	if m.Index >= r.RaftLog.FirstIndex {
		logTerm, err := r.RaftLog.Term(m.Index)
		if err != nil {
			panic("get log term error")
		}
		if logTerm != m.LogTerm {
			index := r.RaftLog.getIndexFromEntryNum(sort.Search(int(r.RaftLog.getPeerIndex(m.Index+1)),
				func(i int) bool { return r.RaftLog.entries[i].Term == logTerm }))
			r.sendAppendResponse(m.From, logTerm, uint64(index), true)
			return
		}
	}

	for _, entry := range m.Entries {
		lastIndex := r.RaftLog.LastIndex()
		if lastIndex >= entry.Index {
			logTerm, err := r.RaftLog.Term(entry.Index)
			if err != nil {
				if err == ErrCompacted {
					r.sendAppendResponse(m.From, None, r.RaftLog.LastIndex(), false)
					return
				}
				_, _ = r.RaftLog.Term(entry.Index)
				panic("get log term error! ")
			}
			if logTerm != entry.Term {
				index := r.RaftLog.getPeerIndex(entry.Index)
				r.RaftLog.entries[index] = *entry
				r.RaftLog.entries = r.RaftLog.entries[:index+1]
				r.RaftLog.stabled = min(r.RaftLog.stabled, uint64(entry.Index-1))
			}
		} else {
			r.RaftLog.entries = append(r.RaftLog.entries, *entry)
		}
	}
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, m.Index+uint64(len(m.Entries)))
	}
	r.becomeFollower(m.Term, m.From)
	r.sendAppendResponse(m.From, None, r.RaftLog.LastIndex(), false)
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term != None && r.Term > m.Term {
		r.sendHeartBeatResponse(m.From, true)
		return
	}
	// r.becomeFollower(m.Term, m.From)
	r.electionElapsed = 0
	r.randomElectionTimeOut = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.sendHeartBeatResponse(m.From, false)
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
	if r.RaftLog.pendingSnapshot != nil {
		return
	}
	metaData := m.Snapshot.Metadata
	if metaData.Index < r.RaftLog.committed {
		r.sendAppendResponse(m.From, None, r.RaftLog.committed, true)
		return
	}
	r.becomeFollower(max(metaData.Term, r.Term), m.From)
	if len(r.RaftLog.entries) > 0 {
		r.RaftLog.entries = nil
	}
	r.RaftLog.FirstIndex = metaData.Index + 1
	r.RaftLog.committed = metaData.Index
	r.RaftLog.applied = metaData.Index
	r.RaftLog.stabled = metaData.Index
	if metaData.ConfState.Nodes != nil {
		r.Prs = make(map[uint64]*Progress)
		for _, pr := range metaData.ConfState.Nodes {
			r.Prs[pr] = &Progress{}
		}
	}
	r.RaftLog.pendingSnapshot = m.Snapshot
	r.sendAppendResponse(m.From, None, r.RaftLog.committed, false)
	log.Printf("send snapshot response :%v", r.RaftLog.committed)
}

func (r *Raft) handleRequestVoteResponse(m pb.Message) {
	if m.Term != None && m.Term < r.Term {
		return
	}
	if !m.Reject {
		r.voteCount++
		r.votes[m.From] = true
	} else {
		r.votes[m.From] = false
	}
	if r.voteCount > uint64(len(r.Prs)/2) && !r.IsLeader() {
		r.becomeLeader()
	} else if len(r.votes)-int(r.voteCount) > len(r.Prs)/2 {
		// log.Infof("request vote")
		r.becomeFollower(r.Term, None)
	}
}

func (r *Raft) handleRequestVote(m pb.Message) {
	reject := false
	if r.Term > m.Term {
		reject = true
	}
	if r.Vote != None && r.Vote != m.From {
		reject = true
	}

	lastIndex := r.RaftLog.LastIndex()
	lastLogTerm, _ := r.RaftLog.Term(lastIndex)
	if lastLogTerm > m.LogTerm || (lastLogTerm == m.LogTerm && lastIndex > m.Index) {
		reject = true
	}

	if !reject {
		r.Vote = m.From
	}

	msg := &pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		From:    r.id,
		To:      m.From,
		Term:    r.Term,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, *msg)
}

func (r *Raft) leaderCommit() {
	match := make(uint64Slice, len(r.Prs))
	i := 0
	for _, peer := range r.Prs {
		match[i] = peer.Match
		i++
	}

	sort.Sort(match)
	halfIndex := match[(len(match)-1)/2]
	if halfIndex > r.RaftLog.committed {
		LogTerm, err := r.RaftLog.Term(halfIndex)
		if err != nil {
			panic("get log term failed!")
		}
		if r.Term == LogTerm {
			r.DPrintf("leader commit")
			r.RaftLog.committed = halfIndex
			r.broadcastAppendEntries()
		}
	}
}

func (r *Raft) handleAppendResponse(m pb.Message) {
	log.Printf("before handleAppendResponsetest: %v %v %v %v", m.From, m.To, r.Prs[m.From].Match, r.Prs[m.From].Next)
	if m.Term < r.Term {
		return
	}

	if m.Reject {
		index := m.Index
		if index == None {
			return
		}
		if m.LogTerm != None {
			index = uint64(r.RaftLog.getIndexFromEntryNum(sort.Search(len(r.RaftLog.entries),
				func(i int) bool { return r.RaftLog.entries[i].Term == m.LogTerm })))
		}
		r.Prs[m.From].Next = index
		r.sendAppend(m.From)
		return
	}

	r.Prs[m.From].Match = m.Index
	r.Prs[m.From].Next = m.Index + 1
	r.leaderCommit()
	if r.leadTransferee == m.From {
		r.handleTransferLeader(m)
	}
	log.Printf("after handleAppendResponsetest: %v %v %v %v", m.From, m.To, r.Prs[m.From].Match, r.Prs[m.From].Next)
}

func (r *Raft) handleTransferLeader(m pb.Message) {
	if _, ok := r.Prs[m.From]; !ok {
		return
	}
	if !r.IsLeader() {
		r.sendMessage(pb.MessageType_MsgTransferLeader, r.Lead)
		return
	}
	if r.Prs[m.From].Next == r.Prs[r.id].Next && r.Prs[m.From].Match == r.Prs[r.id].Match {
		r.leadTransferee = None
		r.sendMessage(pb.MessageType_MsgTimeoutNow, m.From)
		return
	} else {
		r.leadTransferee = m.From
		r.sendAppend(m.From)
	}
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
	if _, ok := r.Prs[id]; !ok {
		// lastIndex := r.RaftLog.LastIndex()
		// firstIndex, _ := r.RaftLog.storage.FirstIndex()
		r.Prs[id] = &Progress{Next: 1}
		// log.Printf("add node last Index %v", r.Prs[id])
		r.DPrintf("add node")
	}
	r.PendingConfIndex = None
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
	if _, ok := r.Prs[id]; ok {
		delete(r.Prs, id)
		if r.IsLeader() {
			r.leaderCommit()
		}
		r.DPrintf("remove node")
	}
	r.PendingConfIndex = None
}

func (r *Raft) IsLeader() bool {
	return r.State == StateLeader
}

func (r *Raft) sendAppendResponse(to uint64, logTerm uint64, index uint64, reject bool) {
	msg := &pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		To:      to,
		From:    r.id,
		Index:   index,
		Reject:  reject,
		Term:    r.Term,
	}
	r.msgs = append(r.msgs, *msg)
}

// -----------------leader functions --------------
func (r *Raft) stepLeader(m pb.Message) {
	switch m.MsgType {
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgPropose:
		r.appendEntries(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handleRequestVoteResponse(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgBeat:
		r.sendHeartBeatMessageToPeers()
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendResponse(m)
	case pb.MessageType_MsgHeartbeatResponse:
		r.sendAppend(m.From)
	case pb.MessageType_MsgTransferLeader:
		r.handleTransferLeader(m)
	}
}

func (r *Raft) broadcastAppendEntries() {
	for id := range r.Prs {
		if id != r.id {
			r.sendAppend(id)
		}
	}
}

func (r *Raft) sendSnapshot(to uint64) {
	snapshot, err := r.RaftLog.storage.Snapshot()
	if err != nil {
		return
	}
	r.msgs = append(r.msgs, pb.Message{
		MsgType:  pb.MessageType_MsgSnapshot,
		Snapshot: &snapshot,
		To:       to,
		From:     r.id,
	})
}

func (r *Raft) appendEntries(m pb.Message) {
	lastIndex := r.RaftLog.LastIndex()
	for i, entry := range m.Entries {
		if entry.EntryType == pb.EntryType_EntryConfChange {
			if r.PendingConfIndex == None {
				r.PendingConfIndex = entry.Index
			} else {
				continue
			}
		}
		entry.Term = r.Term
		entry.Index = lastIndex + uint64(i) + 1
		r.RaftLog.entries = append(r.RaftLog.entries, *entry)
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1
	r.broadcastAppendEntries()
	if len(r.Prs) == 1 {
		r.RaftLog.committed = r.RaftLog.LastIndex()
	}
}

// -----------------follower functions --------------
func (r *Raft) stepFollower(m pb.Message) {
	switch m.MsgType {
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHup:
		r.startElection()
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgTimeoutNow:
		r.startElection()
	case pb.MessageType_MsgTransferLeader:
		r.handleTransferLeader(m)
	}
}

// -----------------candidate functions --------------
func (r *Raft) stepCandidate(m pb.Message) {
	switch m.MsgType {
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handleRequestVoteResponse(m)
	case pb.MessageType_MsgHup:
		r.startElection()
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgTimeoutNow:
		r.startElection()
	case pb.MessageType_MsgTransferLeader:
		r.handleTransferLeader(m)
	}
}

func (r *Raft) DPrintf(format string, a ...interface{}) (n int, err error) {
	var state string
	switch r.State {
	case StateFollower:
		state = "Follower"
	case StateCandidate:
		state = "Candidate"
	case StateLeader:
		state = "Leader"
	default:
		panic("Unknow state")
	}
	msg := fmt.Sprintf(format, a...)
	first, _ := r.RaftLog.storage.FirstIndex()
	log.Printf("Server %d (Term %d, State %s, Lead %d, Applied %d, Commited %d, First %d, stableFirst %d):\n%s",
		r.id, r.Term, state, r.Lead, r.RaftLog.applied, r.RaftLog.committed, r.RaftLog.FirstIndex, first, msg)
	return
}
