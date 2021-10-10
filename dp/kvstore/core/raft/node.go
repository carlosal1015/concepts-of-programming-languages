// Copyright 2018 Johannes Weigend
// Licensed under the Apache License, Version 2.0

// Package raft is an implementation of the RAFT consensus algorithm.
package raft

import (
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

// Node is a node in a Raft consensus cluster. It is called "server" in the original Raft paper.
// Node seems to be more accurate because we can run multiple nodes in a single server process.
type Node struct {
	id             int
	statemachine   *Statemachine
	replicatedLog  *ReplicatedLog
	electionTimer  timercontrol // runs only if the node is FOLLOWER or CANDIDATE
	heartbeatTimer timercontrol // runs only if the node is in LEADER state
	currentTerm    int
	votedFor       *int
	cluster        *Cluster // our cluster
	stopped        bool     // helper to simulate stopped nodes
	mutex          sync.Mutex
}

// NewNode constructor. Id starts with 0 for the first node and should be +1 for the next node.
func NewNode(id int) *Node {
	node := new(Node)
	node.id = id
	node.currentTerm = 0
	node.votedFor = nil
	node.statemachine = NewStatemachine()
	node.replicatedLog = NewReplicatedLog()

	node.electionTimer = createPeriodicTimer(
		func() time.Duration {
			return time.Duration(2000+rand.Intn(1000)) * time.Millisecond
		},
		func() { node.electionTimeout() })

	node.heartbeatTimer = createPeriodicTimer(
		func() time.Duration {
			return time.Duration(1000) * time.Millisecond
		},
		func() { node.heatbeatTimeout() })
	return node
}

// Start starts the node and the election timer. The cluster are the remote interfaces of all other nodes.
func (n *Node) Start(cluster *Cluster) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.stopped = false
	n.cluster = cluster
	n.electionTimer.resetC <- true
}

// Stop stops all running timers and switch to follower state.
func (n *Node) Stop() {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.stopped = true
	n.heartbeatTimer.stopC <- true
	n.electionTimer.stopC <- true
	n.statemachine.Next(FOLLOWER)
}

// =====================================================================================================================
// Election
// =====================================================================================================================

// ElectionTimeout happens when a node receives no heartbeat in a given time period.
func (n *Node) electionTimeout() {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	// make shutdown safe
	if n.stopped {
		return
	}

	n.log(fmt.Sprintf("Election timout."))
	if n.isLeader() {
		panic("The election timeout should not happen, when a node is LEADER.")
	}
	n.startElectionProcess()
}

// StartElectionProcess sends a RequestVote request to other members in the cluster.
// if successful - we get are the new leader in a new term.
func (n *Node) startElectionProcess() {
	n.currentTerm++ // new term starts now -> see 5.2
	n.statemachine.Next(CANDIDATE)
	n.votedFor = nil
	electionWon := n.executeElection()
	if electionWon {
		n.log(fmt.Sprintf("Election won. Now acting as leader."))
		n.switchToLeader()
	} else {
		n.log(fmt.Sprintf("Election was not won. Reset election timer"))
		n.statemachine.Next(FOLLOWER)
		n.electionTimer.resetC <- true // try again, split vote or cluster down
	}
}

// ExecuteElection executes a leader election by sending RequestVote to other nodes.
// for all other nodes in the cluster RequestVote is sent
func (n *Node) executeElection() bool {
	n.log("-> Election")
	n.votedFor = &n.id // vote for ourself

	var wg sync.WaitGroup
	nodes := n.cluster.GetRemoteFollowers(n.id)
	votes := make([]bool, len(nodes))
	wg.Add(len(nodes))
	for i, rpcIf := range nodes {
		go func(w *sync.WaitGroup, i int, rpcIf NodeRPC) {
			term, ok := rpcIf.RequestVote(n.currentTerm, n.id, 0, 0)
			if term > n.currentTerm {
				// todo
			}
			votes[i] = ok
			w.Done()
		}(&wg, i, rpcIf)
	}
	wg.Wait() // wait until all nodes have voted

	// Count votes
	nbrOfVotes := 1 // master votes for itself!
	for _, vote := range votes {
		if vote {
			nbrOfVotes++
		}
	}
	// If more than 50% respond with true - The election was won!
	electionWon := nbrOfVotes >= len(n.cluster.allNodes)/2+1
	n.log(fmt.Sprintf("<- Election: %v", electionWon))
	return electionWon
}

// SwitchToLeader does the state change from CANDIDATE to LEADER.
func (n *Node) switchToLeader() {
	n.statemachine.Next(LEADER)
	n.heartbeatTimer.resetC <- true
	n.electionTimer.stopC <- true
}

// =====================================================================================================================
// Leader only functions
// =====================================================================================================================

// heatbeatTimeout sends the heartbeat to all other nodes in the cluster parallel.
func (n *Node) heatbeatTimeout() {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	// make shutdown safe
	if n.stopped {
		return
	}

	if n.isNotLeader() {
		panic("sendHeartbeat should only run in LEADER state!")
	}

	n.log("-> Heartbeat")

	var wg sync.WaitGroup

	nodes := n.cluster.GetRemoteFollowers(n.id)

	result := make([]bool, len(nodes))
	wg.Add(len(nodes))
	for i, rpcIf := range nodes {
		func(w *sync.WaitGroup, i int, nodeRPC NodeRPC) {
			term, ok := nodeRPC.AppendEntries(n.currentTerm, n.id, 0, 0, nil, 0)
			// See §5.1
			if term > n.currentTerm {
				n.switchToFollower()
			}
			result[i] = ok
			w.Done()
		}(&wg, i, rpcIf)
	}
	wg.Wait() // wait until all nodes have voted

	n.log("<- Heartbeat")
}

// SwitchToFollower switches a LEADER or CANDIDATE to the follower state
func (n *Node) switchToFollower() {
	if n.isLeader() {
		n.heartbeatTimer.stopC <- true
		n.statemachine.Next(FOLLOWER)
	} else if n.isCandidate() {
		n.statemachine.Next(FOLLOWER)
	}
}

// =====================================================================================================================
// Follower RPC - Heartbeat & Replication
// =====================================================================================================================

// AppendEntries implementation is used as heartbeat and log replication.
func (n *Node) AppendEntries(term, leaderID, prevLogIndex, prevLogTermin int, entries []string, leaderCommit int) (currentTerm int, success bool) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if n.stopped {
		return n.currentTerm, false // stopped node
	}

	if term < n.currentTerm {
		return n.currentTerm, false // §5.1
	}

	// see 5.1 - If one servers term is smaller than the others, then it updates its current term to the larger value.
	if term > n.currentTerm {
		n.currentTerm = term
		if n.isLeader() || n.isCandidate() {
			n.switchToFollower()
			return n.currentTerm, false
		}
	}

	// heartbeat received in FOLLOWER -> reset election timer!
	if entries == nil || len(entries) == 0 {
		n.log("Heartbeat received. Reset election timer.")
		n.electionTimer.resetC <- true
	} else {
		// todo: replicate logs
		log.Printf("[%v] AppendEntries replicate logs on Node: %v", n.statemachine.Current(), n.id)

	}

	return n.currentTerm, true
}

// =====================================================================================================================
// Follower RPC - Leader Election
// =====================================================================================================================

// RequestVote is called by candidates to gather votes.
// It returns the current term to update the candidate
// It returns true when the candidate received vote.
func (n *Node) RequestVote(term, candidateID, lastLogIndex, lastLogTerm int) (int, bool) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	// stopped nodes do not vote
	if n.stopped {
		return n.currentTerm, false // stopped node
	}

	n.electionTimer.resetC <- true

	// see RequestVoteRPC receiver implementation 1
	if term < n.currentTerm {
		return n.currentTerm, false
	}
	// see RequestVoteRPC receiver implementation 2
	if n.votedFor != nil && term == n.currentTerm {
		return n.currentTerm, false
	}
	// see 5.1 - If one servers term is smaller than the others, then it updates its current term to the larger value.
	if term > n.currentTerm {
		n.currentTerm = term
		if n.isCandidate() || n.isLeader() {
			n.switchToFollower()
		}
	}

	n.votedFor = &candidateID

	n.log(fmt.Sprintf("RequestVote received from Candidate %v. Vote OK.", candidateID))

	return n.currentTerm, true
}
