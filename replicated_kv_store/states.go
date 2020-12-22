package main

import (
	"context"
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/krithikvaidya/distributed-dns/replicated_kv_store/protos"
)

// ToFollower is called when you get a term higher than your own
func (node *RaftNode) ToFollower(term int32) {

	prevState := node.state
	node.state = Follower
	node.currentTerm = term
	node.votedFor = -1

	// If node was a leader, start election timer. Else if it was a candidate, reset the election timer.
	if prevState == Leader {
		go node.RunElectionTimer()
	} else {
		node.electionResetEvent <- true
	}

}

// ToCandidate is called when election timer runs out
// without heartbeat from leader
func (node *RaftNode) ToCandidate() {

	node.raft_node_mutex.Lock()
	node.state = Candidate
	node.currentTerm++
	node.votedFor = node.replica_id

	//we can start an election for the candidate to become the leader
	node.StartElection()
}

// ToLeader is called when the candidate gets majority votes in election
func (node *RaftNode) ToLeader() {

	// stop election timer since leader doesn't need it
	node.stopElectiontimer <- true

	node.state = Leader

	// initialize nextIndex, matchIndex
	for replica_id := 0; replica_id < len(node.peer_replica_clients); replica_id++ {

		if int32(replica_id) == node.replica_id {
			continue
		}

		node.nextIndex[replica_id] = int32(len(node.log))
		node.matchIndex[replica_id] = int32(0)

	}

	// send no-op for synchronization
	var operation []string
	operation = append(operation, "NO-OP")

	node.log = append(node.log, protos.LogEntry{Term: node.currentTerm, Operation: operation})

	var entries []*protos.LogEntry
	entries = append(entries, &node.log[len(node.log)-1])

	msg := &protos.AppendEntriesMessage{

		Term:         node.currentTerm,
		LeaderId:     node.replica_id,
		PrevLogIndex: int32(len(node.log) - 1),
		PrevLogTerm:  node.log[len(node.log)-1].Term,
		LeaderCommit: node.commitIndex,
		Entries:      entries,
	}

	node.LeaderSendAEs("NO-OP", msg, int32(len(node.log)-1))

	go node.HeartBeats()
}

// RunElectionTimer runs an election if no heartbeat is received
func (node *RaftNode) RunElectionTimer() {
	duration := time.Duration(150+rand.Intn(150)) * time.Millisecond
	//150 - 300 ms random time was mentioned in the paper

	// go node.ElectionStopper(start)

	select {

	case <-time.After(duration): //for timeout to call election

		// if node was a follower, transition to candidate and start election
		// if node was already candidate, restart election
		node.ToCandidate()
		return

	case <-node.stopElectiontimer: //to stop timer
		return

	case <-node.electionResetEvent: //to reset timer when heartbeat/msg received
		go node.RunElectionTimer()
		return

	}
}

// To send AppendEntry to single replica, and retry if needed.
func (node *RaftNode) LeaderSendAE(replica_id int32, upper_index int32, client_obj protos.ConsensusServiceClient, msg *protos.AppendEntriesMessage) {

	response, _ := client_obj.AppendEntries(context.Background(), msg)

	// if err != nil {

	// }

	if response.Success == false {

		if node.state != Leader {
			return
		}

		if response.Term > node.currentTerm {

			node.ToFollower(response.Term)
			return
		}

		// response.Term <= node.currentTerm and it failed

		node.nextIndex[replica_id]--
		tmp := int32(len(node.log))

		if upper_index+1 < tmp {
			tmp = upper_index + 1
		}

		var entries []*protos.LogEntry

		for i := msg.PrevLogIndex; i < tmp; i++ {
			entries = append(entries, &node.log[i])
		}

		new_msg := &protos.AppendEntriesMessage{

			Term:         node.currentTerm,
			LeaderId:     node.replica_id,
			PrevLogIndex: msg.PrevLogIndex - 1,
			PrevLogTerm:  node.log[msg.PrevLogIndex-1].Term,
			LeaderCommit: node.commitIndex,
			Entries:      entries,
		}

		node.LeaderSendAE(replica_id, upper_index, client_obj, new_msg)

	} else {

		node.nextIndex[replica_id] = upper_index + 1
		node.matchIndex[replica_id] = upper_index
		return

	}

}

// Leader sending AppendEntries to all other replicas.
func (node *RaftNode) LeaderSendAEs(msg_type string, msg *protos.AppendEntriesMessage, upper_index int32) {

	replica_id := int32(0)

	for _, client_obj := range node.peer_replica_clients {

		if replica_id == node.replica_id {
			replica_id++
			continue
		}

		go func(node *RaftNode, client_obj protos.ConsensusServiceClient) {

			node.raft_node_mutex.Lock()
			defer node.raft_node_mutex.Unlock()

			node.LeaderSendAE(replica_id, upper_index, client_obj, msg)

		}(node, client_obj)

		replica_id++

	}

}

//HeartBeats is a goroutine that periodically makes leader
//send heartbeats as long as it is the leader
func (node *RaftNode) HeartBeats() {

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {

		<-ticker.C

		if node.state != Leader {
			return
		}

		replica_id := 0

		// send heartbeat
		var entries []*protos.LogEntry

		hbeat_msg := &protos.AppendEntriesMessage{

			Term:         node.currentTerm,
			LeaderId:     node.replica_id,
			PrevLogIndex: node.nextIndex[replica_id] - 1,
			PrevLogTerm:  node.log[node.nextIndex[replica_id]-1].Term,
			LeaderCommit: node.commitIndex,
			Entries:      entries,
		}

		node.LeaderSendAEs("HBEAT", hbeat_msg, int32(len(node.log)))

	}
}

// StartElection is called when candidate is ready to start an election
func (node *RaftNode) StartElection() {

	var received_votes int32 = 1
	replica_id := int32(0)

	for _, client_obj := range node.peer_replica_clients {

		if replica_id == node.replica_id {
			replica_id++
			continue
		}

		go func(node *RaftNode, client_obj protos.ConsensusServiceClient) {

			args := protos.RequestVoteMessage{
				Term:        node.currentTerm,
				CandidateId: node.replica_id,
			}

			//request vote and get reply
			response, err := client_obj.RequestVote(context.Background(), &args)

			if err != nil {

				// by the time the RPC call returns an answer, this replica might have already transitioned to another state.
				node.raft_node_mutex.Lock()
				defer node.raft_node_mutex.Unlock()
				if node.state != Candidate {
					return
				}

				if response.Term > node.currentTerm { // the response node has higher term than current one

					node.ToFollower(response.Term)
					return

				} else if response.Term == node.currentTerm {

					if response.VoteGranted {

						votes := int(atomic.AddInt32(&received_votes, 1))

						if votes*2 > n_replica { // won the Election
							node.ToLeader()
							return
						}

					}

				}

			} else {

				log.Printf("\nError in requesting vote from replica %v: %v", replica_id, err.Error())

			}

		}(node, client_obj)

		replica_id++

	}

	node.raft_node_mutex.Unlock() // was locked in ToCandidate()
	go node.RunElectionTimer()    // begin the timer during which this candidate waits for votes
}