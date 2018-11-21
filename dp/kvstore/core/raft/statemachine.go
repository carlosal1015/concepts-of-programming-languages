package raft

import "fmt"

// State represents the internal raft state. See RAFT paper figure 4.
type State int

const (
	// FOLLOWER is the start state.
	FOLLOWER State = iota
	// CANDIDATE is the candidate state.
	CANDIDATE
	// LEADER is the leader state.
	LEADER
)

// Statemachine encapsulates the current state and ensures only valid state changes are executed.
type Statemachine struct {
	current          State
	validTransitions map[State][]State
}

// NewStatemachine returns a new Statemachine in the FOLLOWER State.
func NewStatemachine() *Statemachine {
	s := new(Statemachine)
	s.current = FOLLOWER
	s.validTransitions = map[State][]State{
		FOLLOWER:  []State{CANDIDATE},
		CANDIDATE: []State{FOLLOWER, CANDIDATE, LEADER},
		LEADER:    []State{FOLLOWER},
	}
	return s
}

// Next advances the state and make sure only valid transitions are made.
func (s *Statemachine) Next(next State) {
	if !s.isValid(next) {
		panic(fmt.Sprintf("State change from %v to %v is not allowed.", s.current, next))
	}
	s.current = next
}

// Current returns the current state.
func (s *Statemachine) Current() State {
	return s.current
}

func (s *Statemachine) isValid(next State) bool {
	nextStates := s.validTransitions[s.current]
	for _, v := range nextStates {
		if v == next {
			return true // found
		}
	}
	return false // not found
}
